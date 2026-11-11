package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
