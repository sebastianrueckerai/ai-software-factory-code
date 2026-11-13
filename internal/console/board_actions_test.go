package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const leakedBoard = "https://user:pass@forge/x answered Authorization: Bearer abc123def456"

// The Approve button answers the merge case of its request through Act: one
// call of approve_merge on that case by the human on rung 3, the work riding
// as Act's Work, which submits the review and signals the workflow.
func TestApproveGoesThroughActOnTheMergeCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	if calls := fmt.Sprint(r.fg.calls); !strings.Contains(calls, "POST /api/v1/repos/sf/code/pulls/9/reviews") || !strings.Contains(calls, "SIGNAL approved t-m #9 by sebastian") {
		t.Fatalf("calls = %s, want the approving review and the workflow's signal: the work ran", calls)
	}
	got, works := actedOnce(t, r)
	if !works {
		t.Fatal("the approval carries no work")
	}
	want := factoryd.ActInput{TaskID: "t-m", CaseID: 7004, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActApproveMerge}, Detail: got.Detail}
	if !equalInput(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
	}
}

func equalInput(a, b factoryd.ActInput) bool {
	return a.TaskID == b.TaskID && a.CaseID == b.CaseID && a.By == b.By && a.Via == b.Via && a.Rung == b.Rung &&
		a.Res.Action == b.Res.Action && a.Res.Note == b.Res.Note
}

// A dependency edge is set through Act: one call of set_dependency on the
// card, which has no case, naming the card and the one it waits for, whose
// work writes today's one dependency_set row.
func TestSetDependencyGoesThroughAct(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	_, s := r.loggedIn(t)
	before := r.events.count()
	say, err := r.c.doSetDependency(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-b", "t-a", false)
	if err != nil || say != "#22 depends on #21 now." {
		t.Fatalf("doSetDependency = %q, %v", say, err)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActSetDependency || got.Res.Args["task_id"] != "t-b" || got.Res.Args["depends_on"] != "t-a" || got.CaseID != 0 || got.Rung != 3 {
		t.Fatalf("Act took %+v, want set_dependency of t-b on t-a with no case on rung 3", got)
	}
	if n := r.events.count() - before; n != 1 {
		t.Fatalf("%d rows written, want the one dependency_set row", n)
	}
}

// A failure of a board action's work that is no refusal of the console's
// carries its words masked: URL userinfo and a bearer value are gone.
func TestBoardWorkFailureIsMasked(t *testing.T) {
	err := maskedErr(errors.New("forge said " + leakedBoard))
	for _, secret := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error %q holds %q", err, secret)
		}
	}
	kept := refuse(http.StatusConflict, "a console sentence")
	if maskedErr(kept) != error(kept) {
		t.Error("a console refusal was rewrapped")
	}
}

// roomyFacts are the facts of a card that stopped with room to draw: three
// attempts of three, a pool and a card draw to take from. over changes them.
func roomyFacts(over func(*factoryd.CaseFacts)) map[string]any {
	f := factoryd.CaseFacts{Attempts: 3, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, SessionLeastS: 300, BudgetEUR: 2, LeftEUR: 1.5, FloorEUR: 0.3,
		CeilingEUR: 10, PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	if over != nil {
		over(&f)
	}
	b, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// seatAsk seats the live ask of card id: a case of kind at the human with facts.
func seatAsk(r *rig, id string, kind factoryd.CaseKind, facts map[string]any) {
	if r.openCases == nil {
		r.openCases = map[string][]factoryd.Case{}
	}
	r.openCases[id] = []factoryd.Case{{ID: 1, TaskID: id, Kind: kind, Status: factoryd.CaseAtHuman, Facts: facts}}
}

// boardFixtures is where the per-kind board fixtures live: one card's life
// per asking kind, in the export's own JSON shape, which the console's
// tests render through the rig.
const boardFixtures = "../factoryd/testdata/fixtures"

// boardFixtureInfo and boardFixtureSource fill what the export's shape does
// not carry, as the ask's own detail keeps it in production: the sentence
// every board fixture's face shows as its info part, and who wrote its
// recommendation.
const (
	boardFixtureInfo   = "The buttons below send the answer to the run as a continue; the note beside them stays on the card."
	boardFixtureSource = "the factory agent"
)

// boardFixtureMoneyKinds mirror the card's money kinds (caseFromAsk), whose
// answer pays from the card's own budget and whose case carries its line.
var boardFixtureMoneyKinds = []factoryd.CaseKind{factoryd.CaseSessionCap, factoryd.CaseAgentTimeCap, factoryd.CaseBudgetFloor,
	factoryd.CaseCheckinShare, factoryd.CaseCheckinFailed}

// seatBoardFixture seats the fixture of one asking kind on the rig: its
// card waiting on a human, its cases at the human with their steps and
// asks, a minute old so their waits read the same on every render. The
// money kinds' cases carry the card's money line, built as caseFromAsk
// writes it, with the card's spend the sum of its sessions' costs.
func seatBoardFixture(t *testing.T, r *rig, name string) factoryd.Fixture {
	t.Helper()
	fx, err := factoryd.LoadFixture(filepath.Join(boardFixtures, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	c := fx.Card
	var spent float64
	for _, s := range fx.Sessions {
		spent += s.CostEUR
	}
	k := c.Kind
	if k == "" {
		k = "agent"
	}
	track := c.Track
	if track == "" {
		track = "direct"
	}
	task := factoryd.Task{ID: c.TaskID, Kind: k, Track: track, Status: "waiting-human", Repository: c.Repository,
		IssueNumber: c.Issue, Objective: c.Objective, BudgetEUR: c.BudgetEUR, CeilingEUR: c.CeilingEUR,
		SpentEUR: spent, CreatedAt: time.Now().Add(-time.Minute)}
	var ask factoryd.FixtureAsk
	for _, a := range fx.Asks {
		ask = a
	}
	question := factoryd.HumanQuestion{Ask: ask.Ask, Why: ask.Why, Info: boardFixtureInfo,
		Recommendation: ask.Recommendation, Source: boardFixtureSource}
	r.tasks.tasks = []factoryd.BoardTask{{Task: task, Escalation: cmp.Or(ask.Reason, ask.Ask), Question: &question}}
	r.caseAsks = map[int64]factoryd.CaseAsk{}
	r.caseSteps = map[int64][]factoryd.CaseStep{}
	cases := []factoryd.Case{}
	for i, fc := range fx.Cases {
		id := int64(i + 1)
		var caseAsk factoryd.FixtureAsk
		for _, a := range fx.Asks {
			if a.CaseKind == fc.Kind {
				caseAsk = a
			}
		}
		if caseAsk.Ask == "" && ask.Ask != "" && len(fx.Cases) == 1 {
			caseAsk = ask
		}
		kase := factoryd.Case{ID: id, TaskID: task.ID, Repository: task.Repository, Kind: factoryd.CaseKind(fc.Kind),
			StateKey: fc.StateKey, Status: factoryd.CaseAtHuman, Problem: fc.Problem, Facts: fc.Facts,
			OpenedAt: time.Now().Add(-70 * time.Second)}
		if slices.Contains(boardFixtureMoneyKinds, kase.Kind) {
			kase.Money = &factoryd.CaseMoney{Kind: fc.Kind, BudgetEUR: task.BudgetEUR, SpentEUR: spent}
		}
		cases = append(cases, kase)
		detail := map[string]any{"info": boardFixtureInfo}
		if caseAsk.Recommendation != "" {
			detail["recommendation"], detail["recommended_by"] = caseAsk.Recommendation, boardFixtureSource
		}
		r.caseAsks[id] = factoryd.CaseAsk{At: time.Now().Add(-70 * time.Second), Detail: detail}
		for _, s := range fc.Steps {
			r.caseSteps[id] = append(r.caseSteps[id], factoryd.CaseStep{Rung: s.Rung, Actor: s.Actor,
				Found: s.Found, Decision: s.Decision, Grounds: s.Grounds, Outcome: s.Outcome})
		}
	}
	r.openCases = map[string][]factoryd.Case{task.ID: cases}
	return fx
}

var actionButtons = regexp.MustCompile(`name="action" value="(\w+)"`)

// buttonsOf are the action buttons of card id's answer form, in order.
func buttonsOf(t *testing.T, r *rig, id string) []string {
	t.Helper()
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	var out []string
	for _, m := range actionButtons.FindAllStringSubmatch(escalationOf(t, body, id), -1) {
		out = append(out, m[1])
	}
	return out
}

// The face's buttons are the actions Offered lists for the human on the
// case's facts: on the check-in's seed gate stop, resume, continue,
// handover, reverify, extend, fund, raise_ceiling, successor, replan, close
// and note (rung 3 adds raise_ceiling and close to rung 1's list, withdraw
// is the factory's own). The action the ask preselected leads.
func TestFaceButtonsAreOfferedAtTheHumansRung(t *testing.T) {
	seed := factoryd.CaseFacts{Kind: factoryd.CaseGateStop, Gate: "review", Attempts: 1, SessionCap: 3, AgentTimeCapS: 3600,
		BudgetEUR: 5, SpentEUR: 1, LeftEUR: 4, FloorEUR: 0.3, CeilingEUR: 7, PROpen: true, Head: "c1", Checks: "green",
		Findings:     []string{"3f2a1c9b0e44"},
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		PoolLeft:     policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}
	b, _ := json.Marshal(seed)
	var facts map[string]any
	_ = json.Unmarshal(b, &facts)
	want := []string{"resume", "continue", "handover", "reverify", "extend", "fund", "raise_ceiling", "successor", "replan", "close", "note"}

	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, facts)
	if got := buttonsOf(t, r, "t-g"); !slices.Equal(got, want) {
		t.Fatalf("the face's buttons = %v, want Offered's %v", got, want)
	}

	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"preselected": map[string]any{"action": "extend"}, "recommended_by": "the factory agent"}}}
	if got, wantLead := buttonsOf(t, r, "t-g"), append([]string{"extend"}, slices.DeleteFunc(slices.Clone(want), func(n string) bool { return n == "extend" })...); !slices.Equal(got, wantLead) {
		t.Fatalf("the face's buttons with extend preselected = %v, want %v", got, wantLead)
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	if !strings.Contains(escalationOf(t, body, "t-g"), `aria-label="Extend, recommended by the factory agent"`) {
		t.Errorf("the recommended button does not name who recommended it:\n%s", escalationOf(t, body, "t-g"))
	}
}

// One send: the form posts the case, the action its button names with that
// action's inputs, the fund beside it and the note, and Act is called once with
// one resolution: handover on the same model, a draw of 1.50 EUR and the
// note, which the console posts as the human's comment.
func TestOneSendCarriesDecisionFundAndNote(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(nil))
	rec := r.do(post("/tasks/t-g/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "case_id": {"1"},
		"action": {"handover"}, "handover.model": {"same"}, "extend.sessions": {"9"}, "fund_eur": {"1.50"}, "note": {"go on with the cache"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body)
	}
	got, posts := actedOnce(t, r)
	want := factoryd.ActInput{TaskID: "t-g", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActHandover, Args: map[string]any{"model": "same"}, Fund: &policy.Draw{EUR: 1.5}, Note: "go on with the cache"}}
	got.Detail = nil
	if !reflect.DeepEqual(got, want) || !posts {
		t.Fatalf("Act took %+v (post %v), want %+v with a post", got, posts, want)
	}
	if c := r.fg.comments["/api/v1/repos/sf/code/issues/21/comments"]; c != "go on with the cache" {
		t.Fatalf("comment = %q, want the note", c)
	}
}

// With only the actions every case takes to offer (close, note, continue,
// replan), the face names why the others do not apply.
func TestFaceNamesTheRefusalsWhenNothingFits(t *testing.T) {
	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(func(f *factoryd.CaseFacts) { f.Final, f.Running = "failed", true }))
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	esc := escalationOf(t, body, "t-g")
	if !strings.Contains(esc, `<p class="refusal"><b>resume</b>: a session is running`) || !strings.Contains(esc, `name="action" value="close"`) {
		t.Fatalf("the face =\n%s\nwant resume's refusal and the close", esc)
	}
	if strings.Contains(esc, "Go on") {
		t.Errorf("a generic Go on survives:\n%s", esc)
	}
}

// restart_plan closes the plan at rest as its Close does, with the note as
// the waiver reason, then files its objective anew; a project whose process has no
// plan track files nothing, and the answer says so after the close.
func TestRestartPlanClosesThePlanAndSaysWhenTheNewOneCannotBeFiled(t *testing.T) {
	r, _ := newWritesRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-br", Kind: "agent", Status: "done", Track: policy.TrackPlan,
		Repository: "sf/code", IssueNumber: 50, Objective: "Build the stats.\n\nCount the hits."}})
	r.tasks.mu.Unlock()
	var closes []string
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, bool) error { return nil }
	r.c.cfg.CloseHeldPlan = func(_ context.Context, id, by, note string, _ bool) (int, string, bool, error) {
		closes = append(closes, id+" by "+by+": "+note)
		return 0, factoryd.PlanCloseWords(by, note, false), false, nil
	}
	_, s := r.loggedIn(t)
	plan, _, _ := r.tasks.Task(t.Context(), "t-br")
	say, err := r.c.restartPlan(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, plan, "split it in two")
	const want = "Closed plan #50. The new plan could not be filed: Project ai-software-factory-code follows process maintenance, which has no plan track: " +
		"file the card on track direct. File it again with the shape in your note."
	if err != nil || say != want {
		t.Fatalf("restartPlan = %q, %v; want %q", say, err, want)
	}
	if len(closes) != 1 || closes[0] != "t-br by sebastian: split it in two" || len(r.fg.patched) != 1 || len(r.fg.issues) != 0 {
		t.Fatalf("closes %q, patched %q, issues %v; want the plan closed once with the note and nothing filed", closes, r.fg.patched, r.fg.issues)
	}
}

// The case of an agent's ask leads with the ask's own answers as the primary
// buttons; the table's actions follow with the quiet weight, an action with an
// argument rendering its input beside its button (raise_ceiling's amount), and
// the raw reason is not repeated: the ask shows once.
func TestAnAsksOwnAnswersLeadAndTheTablesActionsFollow(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-ch", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "add the feed"},
		Escalation: "The agent asks: which store?\nWhy: two fit", Choices: []string{"Postgres", "SQLite"}}}
	r.openCases = map[string][]factoryd.Case{"t-ch": {{ID: 1, TaskID: "t-ch", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman, Problem: "which store?",
		Facts: roomyFacts(nil)}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	esc := escalationOf(t, body, "t-ch")
	pg, sq, raise := strings.Index(esc, `name="premade" value="Postgres" class="primary"`), strings.Index(esc, `name="premade" value="SQLite" class="primary"`),
		strings.Index(esc, `name="action" value="raise_ceiling" class="secondary"`)
	if pg < 0 || sq < pg || raise < sq {
		t.Fatalf("the face =\n%s\nwant the ask's answers first as primary buttons, then the table's as secondary", esc)
	}
	if !strings.Contains(esc[:raise], `name="raise_ceiling.eur"`) || strings.Contains(esc, `class="reason"`) || strings.Count(esc, "which store?") != 1 {
		t.Errorf("raise_ceiling lacks its amount input beside it, or the ask shows more than once:\n%s", esc)
	}
}

// A recommendation that names an action in its words leads nothing: the
// preselected action's name does.
func TestTheRecommendedButtonIsTheCarriedActionNeverParsedText(t *testing.T) {
	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(nil))
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"recommendation": "close: the work is done"}}}
	if got := buttonsOf(t, r, "t-g"); got[0] == "close" {
		t.Fatalf("buttons %v: a recommendation's words led with close", got)
	}
}

// A render reads the offers the ask stored and nothing else: the facts
// would offer more, the stored list is what shows.
func TestARenderShowsTheStoredOffersOnly(t *testing.T) {
	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(nil))
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{
		"offers": []factoryd.AskOffer{{Value: "close", Label: "Close", Effect: "End the card and close its issue."}, {Value: "note", Label: "Note", Effect: "Record your note."}}}}}
	if got := buttonsOf(t, r, "t-g"); !slices.Equal(got, []string{"close", "note"}) {
		t.Fatalf("buttons = %v, want the stored close and note", got)
	}
}

// The chat's answer tool takes a merge case of a card in review: the case
// answer has one gate, Act, which the rig records.
func TestAnswerToolTakesAMergeCaseOfACardInReview(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 5, PRNumber: 9, Objective: "Add the index."}}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 4, Kind: factoryd.ApprovalMerge, TaskID: "t-m", Project: "sf/code",
		PRNumber: 9, Status: "pending", Spec: json.RawMessage(`{"risk_class":"low","head_sha":"h1"}`)})
	_, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-m", CaseID: 7004, Action: factoryd.ActApproveMerge})
	if len(r.acted) != 1 || r.acted[0].CaseID != 7004 || r.acted[0].Res.Action != factoryd.ActApproveMerge {
		t.Fatalf("err %v, Act calls %+v; want one approve_merge on case 7004", err, r.acted)
	}
}

// The drawer of a card with a live case answers from the case's own form:
// no old answer endpoint, no value buttons, and Close once.
func TestTheDrawerAnswersFromTheCasesForm(t *testing.T) {
	r, _ := newRigWithEvents(t)
	q := &factoryd.HumanQuestion{Ask: "The call cap is reached: which way on?",
		Choices: []factoryd.QuestionChoice{{Value: "grant", Label: "Grant one session"}, {Value: "park", Label: "Park it"}}}
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"},
		Escalation: "the session cap is reached", Question: q}}
	seatAsk(r, "t-q", factoryd.CaseSessionCap, roomyFacts(nil))
	ck, _ := r.loggedIn(t)
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-q/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(drawer, `action="/tasks/t-q/resolve"`) || strings.Contains(drawer, `action="/tasks/t-q/answer"`) || strings.Contains(drawer, `name="value" value=`) {
		t.Errorf("the drawer keeps the old forms beside the case:\n%s", drawer)
	}
	if n := strings.Count(drawer, `value="close"`) + strings.Count(drawer, `/tasks/t-q/close"`); n != 1 {
		t.Errorf("the drawer shows Close %d times, want once", n)
	}
}

// new_card on a card's case carries every field the plain filing takes: the
// size, the carried pull request and the roadmap item reach the issue.
func TestNewCardOnACaseCarriesEveryField(t *testing.T) {
	r, _ := newWritesRig(t)
	_, err := runTool(t, r, "new_card", "member", toolInput{TaskID: "t-cap", Title: "Fix the cause", Body: "It is the cache.", Track: "direct",
		TrackReason: "one change", Acceptance: []string{"The cache holds"}, Size: "small", CarryPR: 9, RoadmapItem: 12})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActFileCard || got.Res.Args["carry_pr"] != int64(9) && got.Res.Args["carry_pr"] != 9 {
		t.Fatalf("Act took %+v, want file_card carrying the pull request", got)
	}
	body, _ := r.fg.issues[0]["body"].(string)
	for _, want := range []string{"It is the cache.", "Size: small", "Carries: #9", "Roadmap: #12", "Filed from #14"} {
		if !strings.Contains(body, want) {
			t.Errorf("the issue's body lacks %q:\n%s", want, body)
		}
	}
}

// raise_ceiling's amount rides its own input: one send of raise_ceiling with
// 2.50 EUR is one Act call whose args carry the amount.
func TestRaiseCeilingTakesItsOwnAmount(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(nil))
	rec := r.do(post("/tasks/t-g/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "case_id": {"1"},
		"action": {"raise_ceiling"}, "raise_ceiling.eur": {"2.50"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActRaiseCeiling || got.Res.Args["eur"] != 2.5 || got.Res.Fund != nil {
		t.Fatalf("Act took %+v, want raise_ceiling of 2.5 EUR and no fund", got.Res)
	}
}

// restart_plan closes the done plan as its Close does and files its
// objective anew on the plan track, the note the shape to take.
func TestRestartPlanFilesTheNewPlan(t *testing.T) {
	r, _ := newWritesRig(t)
	withLinkshort(r)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-br", Kind: "agent", Status: "done", Track: policy.TrackPlan,
		Repository: "linkshort/linkshort", Objective: "Build the stats.\n\nCount the hits."}})
	r.tasks.mu.Unlock()
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, bool) error { return nil }
	var leftToAct bool
	r.c.cfg.CloseHeldPlan = func(ctx context.Context, _, by, note string, _ bool) (int, string, bool, error) {
		leftToAct = factoryd.CaseLeftToAct(ctx)
		return 0, factoryd.PlanCloseWords(by, note, false), false, nil
	}
	_, s := r.loggedIn(t)
	plan, _, _ := r.tasks.Task(t.Context(), "t-br")
	say, err := r.c.restartPlan(t.Context(), actor{s: s, via: factoryd.ViaButton}, plan, "split it in two")
	if err != nil || say != "Closed plan t-br. Filed linkshort/linkshort#42 as the new plan." {
		t.Fatalf("restartPlan = %q, %v", say, err)
	}
	if !leftToAct {
		t.Error("the restart's close ran with its case left to Act: it ended the case itself")
	}
	if len(r.fg.issues) != 1 || r.fg.issues[0]["title"] != "Build the stats." || fmt.Sprint(r.fg.issues[0]["labels"]) != "[24]" ||
		!strings.HasPrefix(r.fg.issues[0]["body"].(string), "Count the hits.\n\nThis restarts plan t-br, which was closed. The shape to take:\n\nsplit it in two") {
		t.Fatalf("issues filed = %v, want the objective anew on the plan track with the shape", r.fg.issues)
	}
}

// A case whose ask stores no offers renders the case's own form with the fund
// and the note and no table button: the old answer, fund and extend forms are
// gone for a cased card.
func TestACaseWithNoStoredOffersRendersNoOldForms(t *testing.T) {
	r := newRig(t)
	r.noStoredOffers = true
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseSessionCap, roomyFacts(nil))
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	esc := escalationOf(t, body, "t-g")
	if strings.Contains(esc, `action="/tasks/t-g/answer"`) || strings.Contains(esc, `action="/tasks/t-g/fund"`) || strings.Contains(esc, `action="/tasks/t-g/extend"`) ||
		!strings.Contains(esc, `action="/tasks/t-g/resolve"`) || strings.Contains(esc, `name="action"`) {
		t.Fatalf("the face of a case with no stored offers =\n%s\nwant the resolve form alone, with no table button", esc)
	}
}
