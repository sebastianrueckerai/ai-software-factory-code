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
	if rec := r.do(post("/tasks/t-m/resolve", askForm(s.csrf, 4, factoryd.ActApproveMerge)), ck); rec.Code != http.StatusSeeOther {
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

// A send names the ask it answers: one with no case is refused with the
// way back, and Act takes nothing.
func TestASendWithNoCaseIsRefused(t *testing.T) {
	r := newRig(t)
	seatBoardFixture(t, r, "board-agent-escalate.json")
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-board-escalate/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"action": {"note"}, "note.note": {"the schema change is mine to make"}}), ck)
	if want := "This answer names no question of the card. Reload the board and press the answer again."; rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), want) || len(r.acted) != 0 {
		t.Fatalf("a send with no case = %d %q, Act took %+v; want 400 %q and nothing acted", rec.Code, rec.Body, r.acted, want)
	}
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
// attempts of three, a pool, a card draw and the project's month to take
// from. over changes them.
func roomyFacts(over func(*factoryd.CaseFacts)) map[string]any {
	f := factoryd.CaseFacts{Attempts: 3, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, SessionLeastS: 300, BudgetEUR: 2, LeftEUR: 1.5, FloorEUR: 0.3,
		CeilingEUR: 10, MonthLeftEUR: 50, PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
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
	// A plan card's ask is its plan approval: the card waits on its
	// revision's questions, whose cases the fake opens with the planner's
	// recommendation, and the pending approval whose body the card shows.
	if c.Track == "plan" {
		task.Process = "spec"
		labels := make([]string, len(ask.Choices))
		for i, ch := range ask.Choices {
			labels[i] = ch.Label
		}
		r.tasks.tasks = []factoryd.BoardTask{{Task: task}}
		r.tasks.plans = map[string][]factoryd.PlanRevision{c.TaskID: {planRevision(c.TaskID, 2, "sha-2")}}
		r.tasks.questions = map[string][]factoryd.PlanQuestion{c.TaskID + " 2": {
			{Text: ask.Ask, Choices: labels, Recommended: ask.Recommendation, Grounds: ask.Why}}}
		r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: c.TaskID,
			Project: c.Repository, Status: "pending", Spec: json.RawMessage(`{"revision":2,"sha256":"sha-2"}`)})
		r.caseAsks = map[int64]factoryd.CaseAsk{500: {At: time.Now().Add(-70 * time.Second), Detail: map[string]any{}}}
		return fx
	}
	question := factoryd.HumanQuestion{Ask: ask.Ask, Why: ask.Why, Info: boardFixtureInfo,
		Recommendation: ask.Recommendation, Source: boardFixtureSource, Choices: ask.Choices}
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
			// a rung recommends the face's first answer, as the ask row stores it
			face := factoryd.FaceOf(kase, factoryd.Facts{CaseFacts: factoryd.FactsOf(kase), Card: task})
			detail["preselected"] = map[string]any{"action": face.Answers[0].Key()}
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

var actionButtons = regexp.MustCompile(`data-answer="([^"]+)"`)

// buttonsOf are the answers of card id's ask, in order.
func buttonsOf(t *testing.T, r *rig, id string) []string {
	t.Helper()
	ck, _ := r.loggedIn(t)
	var out []string
	for _, m := range actionButtons.FindAllStringSubmatch(escalationOf(t, r, ck, id), -1) {
		out = append(out, m[1])
	}
	return out
}

// The face's buttons are the register's answers for the case on its facts:
// on the check-in's seed review gate stop, check the work again, go on with
// a note, send back to the planner and end the card. An answer the ask
// preselected leads, and its line names who recommended it.
func TestFaceButtonsAreTheRegistersAnswers(t *testing.T) {
	seed := factoryd.CaseFacts{Kind: factoryd.CaseGateStop, Gate: "review", Attempts: 1, SessionCap: 3, AgentTimeCapS: 3600,
		BudgetEUR: 5, SpentEUR: 1, LeftEUR: 4, FloorEUR: 0.3, CeilingEUR: 7, PROpen: true, Head: "c1", Checks: "green",
		Findings:     []string{"3f2a1c9b0e44"},
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		PoolLeft:     policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}
	b, _ := json.Marshal(seed)
	var facts map[string]any
	_ = json.Unmarshal(b, &facts)

	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, facts)
	if got, want := buttonsOf(t, r, "t-g"), []string{"reverify", "continue", "replan", "close"}; !slices.Equal(got, want) {
		t.Fatalf("the face's buttons = %v, want the register's %v", got, want)
	}

	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"preselected": map[string]any{"action": "replan"}, "recommended_by": "the factory agent"}}}
	if got, want := buttonsOf(t, r, "t-g"), []string{"replan", "reverify", "continue", "close"}; !slices.Equal(got, want) {
		t.Fatalf("the face's buttons with replan preselected = %v, want %v", got, want)
	}
	ck, _ := r.loggedIn(t)
	if !strings.Contains(escalationOf(t, r, ck, "t-g"), `<p class="o-by">suggested by the factory agent</p>`+"\n"+`<button type="button" class="pick" aria-expanded="false" aria-controls="f-t-g-1-replan-note-set" data-press="replan">Send back to the planner</button>`) {
		t.Errorf("the recommended button does not name who recommended it:\n%s", escalationOf(t, r, ck, "t-g"))
	}
}

// One send carries one answer: the form posts the case, the action and
// that action's own fields, and Act is called once with one resolution:
// handover on the same model. Another answer's fields and a note the
// answer does not take ride nothing: the comment is the answer's own
// sentence (Resolution.Sentence), not the stray note.
func TestOneSendCarriesTheDecisionAlone(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(nil))
	rec := r.do(post("/tasks/t-g/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "case_id": {"1"},
		"action": {"handover"}, "handover.model": {"same"}, "extend.sessions": {"9"}, "extend.money": {"yes"}, "note": {"go on with the cache"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	want := factoryd.ActInput{TaskID: "t-g", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActHandover, Args: map[string]any{"model": "same"}}}
	got.Detail = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
	}
	if c := r.fg.comments["/api/v1/repos/sf/code/issues/21/comments"]; c != "“Switch the model”" {
		t.Fatalf("comment = %q, want the answer's sentence", c)
	}
}

// An answer the facts refuse never shows. At the session cap with no pull
// request open, the gate stop offers neither a check of the work nor a
// session with a note: send back to the planner and end the card stay.
// Once a session runs, the note goes to it, and the planner waits.
func TestTheFaceOffersNoAnswerItsFactsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		over func(*factoryd.CaseFacts)
		want []string
	}{
		{"at the cap", nil, []string{"replan", "close"}},
		{"a session runs", func(f *factoryd.CaseFacts) { f.Running = true }, []string{"continue", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
			seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(tc.over))
			if got := buttonsOf(t, r, "t-g"); !slices.Equal(got, tc.want) {
				t.Fatalf("buttons = %v, want %v", got, tc.want)
			}
		})
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
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, string, bool) error { return nil }
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

// The case of an agent's ask leads with the agent's own answers as its
// buttons, then Not now and End the card. Its recommendation's words show
// as the agent's view in the opened card's story and preselect none of
// them: words name no button. The small card shows the ask once and the
// raw reason never.
func TestAnAsksOwnAnswersLeadAndTheRegistersFollow(t *testing.T) {
	r := newRig(t)
	reason := "The agent asks: which store?\nWhy: two fit\nRecommendation: SQLite, one file is enough"
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-ch", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "add the feed"},
		Escalation: reason, Choices: []string{"Postgres", "SQLite"}}}
	facts := roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts = 1 })
	facts["choices"], facts["reason"] = []any{"Postgres", "SQLite"}, reason
	r.openCases = map[string][]factoryd.Case{"t-ch": {{ID: 1, TaskID: "t-ch", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman, Problem: "which store?",
		Facts: facts}}}
	ck, _ := r.loggedIn(t)
	esc := escalationOf(t, r, ck, "t-ch")
	if got, want := buttonsOf(t, r, "t-ch"), []string{"say:Postgres", "say:SQLite", "note", "close"}; !slices.Equal(got, want) || strings.Contains(esc, "suggested") {
		t.Fatalf("answers = %q, want %q and none suggested:\n%s", got, want, esc)
	}
	if !strings.Contains(esc, `<li>The agent&#39;s view: <span class="quote">SQLite, one file is enough</span></li>`) || strings.Contains(esc, `class="reason"`) || strings.Count(esc, "hich store?") != 1 {
		t.Errorf("the opened card lacks the agent's view, or shows the ask more than once:\n%s", esc)
	}
	if face := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "t-ch"); strings.Contains(face, "The agent asks") ||
		strings.Count(face, "hich store?") != 1 || strings.Contains(face, `class="by"`) {
		t.Errorf("the small card shows the raw reason, the ask other than once, or a pick:\n%s", face)
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

// A render shows every answer the card holds now in its ask block: the
// offers the ask stored first, in their order, then an answer that holds
// since the face was written. No fold holds an answer of the ask.
func TestARenderShowsEveryAnswerTheCardHoldsNow(t *testing.T) {
	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseGateStop, roomyFacts(nil))
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"ask": "Go on?", "story": factoryd.Story{},
		"offers": []factoryd.AskOffer{{Value: "close", Label: "End the card", Effect: "Ends the card."}, {Value: "note", Label: "Not now", Effect: "The card stays as it is."}}}}}
	// the gate stop's answers are reverify, continue, replan and close; with
	// every session used and no pull request only replan and close hold
	if got := buttonsOf(t, r, "t-g"); !slices.Equal(got, []string{"close", "note", "replan"}) {
		t.Fatalf("buttons = %v, want the stored close and note, then replan", got)
	}
	ck, _ := r.loggedIn(t)
	if _, folds, _ := strings.Cut(drawer(t, r, ck, "t-g", "overview"), `<div class="o-folds">`); strings.Contains(folds, "Send back to the planner") {
		t.Errorf("a fold holds an answer of the ask:\n%s", folds)
	}
}

// A card the review stopped for repeated findings holds more sessions and
// its end as answers. An ask stored without them shows both in its ask
// block, after the stored offer, and under no fold.
func TestARepeatedFindingsCardShowsItsLateAnswersInTheAsk(t *testing.T) {
	r := newRig(t)
	r.tasks.set("the review repeats", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseFindingsRepeated, roomyFacts(nil))
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"ask": "Go on?", "story": factoryd.Story{},
		"offers": []factoryd.AskOffer{{Value: "note", Label: "Not now", Effect: "The card stays as it is."}}}}}
	got := buttonsOf(t, r, "t-g")
	if len(got) < 3 || got[0] != "note" || !slices.Contains(got[1:], "extend") || !slices.Contains(got[1:], "close") {
		t.Fatalf("buttons = %v, want the stored note first, then extend and close among the answers the card holds now", got)
	}
	ck, _ := r.loggedIn(t)
	ask, folds, _ := strings.Cut(drawer(t, r, ck, "t-g", "overview"), `<div class="o-folds">`)
	for _, label := range []string{"Give more room", "End the card"} {
		if !strings.Contains(ask, label) || strings.Contains(folds, label) {
			t.Errorf("“%s” stands outside the ask block or under a fold:\nthe ask:\n%s\nthe folds:\n%s", label, ask, folds)
		}
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

// restart_plan closes the done plan as its Close does and files its
// objective anew on the plan track, continuing the old plan's issue, the
// note the shape to take.
func TestRestartPlanFilesTheNewPlan(t *testing.T) {
	r, _ := newWritesRig(t)
	withLinkshort(r)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-br", Kind: "agent", Status: "done", Track: policy.TrackPlan,
		Repository: "linkshort/linkshort", IssueNumber: 30, Objective: "Build the stats.\n\nCount the hits."}})
	r.tasks.mu.Unlock()
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, string, bool) error { return nil }
	var leftToAct bool
	r.c.cfg.CloseHeldPlan = func(ctx context.Context, _, by, note string, _ bool) (int, string, bool, error) {
		leftToAct = factoryd.CaseLeftToAct(ctx)
		return 0, factoryd.PlanCloseWords(by, note, false), false, nil
	}
	_, s := r.loggedIn(t)
	plan, _, _ := r.tasks.Task(t.Context(), "t-br")
	say, err := r.c.restartPlan(t.Context(), actor{s: s, via: factoryd.ViaButton}, plan, "split it in two")
	if err != nil || say != "Closed plan #30. Filed linkshort/linkshort#42 as the new plan." {
		t.Fatalf("restartPlan = %q, %v", say, err)
	}
	if !leftToAct {
		t.Error("the restart's close ran with its case left to Act: it ended the case itself")
	}
	if len(r.fg.issues) != 1 || r.fg.issues[0]["title"] != "Build the stats." || fmt.Sprint(r.fg.issues[0]["labels"]) != "[24]" ||
		!strings.HasPrefix(r.fg.issues[0]["body"].(string), "Continues #30.\n\nCount the hits.\n\nIt restarts the plan, which was closed. The shape to take:\n\nsplit it in two") {
		t.Fatalf("issues filed = %v, want the objective anew on the plan track with the shape", r.fg.issues)
	}
}

// A case whose ask stores no face renders the register's face on the
// case's own facts, each answer a send of its own to the case's resolve:
// at the session cap, give more room, end the card and not now. The old
// answer, fund and extend forms are gone for a cased card.
func TestACaseWithNoStoredFaceRendersTheRegistersAndNoOldForms(t *testing.T) {
	r := newRig(t)
	r.tasks.set("the gate stopped", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Gate"})
	seatAsk(r, "t-g", factoryd.CaseSessionCap, roomyFacts(nil))
	ck, _ := r.loggedIn(t)
	esc := escalationOf(t, r, ck, "t-g")
	if strings.Contains(esc, `action="/tasks/t-g/answer"`) || strings.Contains(esc, `action="/tasks/t-g/fund"`) || strings.Contains(esc, `action="/tasks/t-g/extend"`) ||
		strings.Count(esc, `action="/tasks/t-g/resolve"`) != 3 || !strings.Contains(esc, `name="note.note"`) {
		t.Fatalf("the face of a case with no stored face =\n%s\nwant the resolve forms alone, Not now's note its own field", esc)
	}
	if got := buttonsOf(t, r, "t-g"); !slices.Equal(got, []string{"extend", "close", "note"}) {
		t.Fatalf("buttons = %v, want the register's extend, close and note", got)
	}
}
