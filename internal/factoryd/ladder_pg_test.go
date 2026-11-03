package factoryd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tripRig is a runner on Postgres with card t1, a large agent card of
// demo-app/demo-app on the direct track, running on issue 12 with a
// budget of 3 EUR, under the policy of the act rig with session floors of
// 0.30 EUR. Forgejo answers head h2's checks with checks, takes comments
// on issue 12 (ids 501 on) and its labels, and knows nothing else. The
// workflows record every signal.
type tripRig struct {
	r       *Runner
	s       *PGStore
	forge   *tripForge
	signals *caseSignals
}

type tripForge struct {
	mu       sync.Mutex
	checks   string // h2's combined state: success or failure
	comments []string
}

func (f *tripForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const issue = "/api/v1/repos/demo-app/demo-app/issues/12"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == issue+"/comments":
			b, _ := io.ReadAll(r.Body)
			f.comments = append(f.comments, string(b))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.comments))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/demo-app/demo-app/commits/h2/status":
			fmt.Fprintf(w, `{"state":%q,"statuses":[{"status":%[1]q,"context":"ci / test"}]}`, f.checks)
		case strings.HasPrefix(r.URL.Path, issue+"/labels"):
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTripRig(t *testing.T, checks string) tripRig {
	t.Helper()
	ctx := context.Background()
	s := cardCaseStore(t, "ladder")
	if err := s.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 12, Track: policy.TrackDirect, Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	v.Document.Budget.SessionFloorCheapEUR, v.Document.Budget.SessionFloorFrontierEUR = 0.3, 0.3
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	signals, forge := &caseSignals{}, &tripForge{checks: checks}
	cfg.Workflows = signals
	objects := &fakeObjectStore{}
	r := &Runner{store: s, objects: objects, cfg: cfg, timeline: &Timeline{Objects: objects, Events: s, Store: s},
		forgejo: &Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"}}
	return tripRig{r: r, s: s, forge: forge, signals: signals}
}

// atHead puts pull request #9 on t1 with head h2, whose newest review
// approved it when approved.
func (g tripRig) atHead(t *testing.T, approved bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	verdict := "concerns"
	if approved {
		verdict = "approve"
	}
	for _, e := range []Event{
		{Kind: EventGateHead, Detail: map[string]any{"gate": GateMerge, "head": "h2"}},
		{Kind: EventModelReview, Detail: map[string]any{"commit": "h2", "round": 1, "reviewed": true, "verdict": verdict}},
	} {
		e.Repository, e.TaskID, e.IssueNumber, e.Actor, e.Via = "demo-app/demo-app", "t1", 12, FactoryDecider, ViaFactoryd
		if _, err := g.s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func (g tripRig) trip(t *testing.T, reason string) TripOutcome {
	t.Helper()
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: reason})
	if err != nil {
		t.Fatalf("OpenTrip: %v", err)
	}
	return out
}

// asks are t1's question_asked rows, oldest first.
func (g tripRig) asks(t *testing.T) []Event { return actRig{s: g.s}.rows(t, EventQuestionAsked) }

const tripCap = capReached + "\nlast: no session produced a result"

// A session cap on an open pull request whose head h2 is green and
// approved: rung 0 routes it to the approval in place. The case reads
// resolved by factoryd on rung 0 with route_approval, its one step is
// rung 0's, no comment is posted and no question is asked. The run hears
// of it.
func TestOpenTripRoutesAGreenApprovedHeadToApproval(t *testing.T) {
	g := newTripRig(t, "success")
	g.atHead(t, true)
	out := g.trip(t, tripCap)

	if out.Resolution == nil || out.Resolution.Action != ActRouteApproval || out.Comment != 0 || out.Rung != 0 || out.CaseID == 0 {
		t.Fatalf("outcome = %+v, want route_approval on rung 0 with no comment", out)
	}
	c := caseByID(t, g.s, out.CaseID)
	if c.Kind != CaseSessionCap || c.StateKey != "h2" || c.Status != CaseResolved ||
		!reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActRouteApproval}) {
		t.Fatalf("case = %+v, resolution %+v; want the session cap at h2 resolved by factoryd on rung 0 with route_approval", c, c.Resolution)
	}
	steps := caseStepsOf(t, g.s, c.ID)
	if len(steps) != 1 || steps[0].Rung != 0 || steps[0].Actor != FactoryDecider || steps[0].Decision != ActRouteApproval || steps[0].Outcome != StepResolved {
		t.Fatalf("steps = %+v, want rung 0's resolving step alone", steps)
	}
	if len(g.forge.comments) != 0 || len(g.asks(t)) != 0 {
		t.Fatalf("comments %q, asks %d; want none", g.forge.comments, len(g.asks(t)))
	}
	if got := g.signals.got; len(got) != 1 || got[0].signal != SignalCaseResolved || got[0].sig.Case == nil || got[0].sig.Case.CaseID != c.ID {
		t.Fatalf("signals = %+v, want case_resolved of case %d", got, c.ID)
	}
}

// A red head's checks run again once: the first trip resolves with
// rerun_checks, after which the facts read RerunOnHead. The same stop on
// the same head opens the case that follows, rung 0 hands it up, and the
// human is asked: one comment in all.
func TestOpenTripRerunsRedChecksOnceThenAsks(t *testing.T) {
	g := newTripRig(t, "failure")
	g.atHead(t, false)
	first := g.trip(t, tripCap)
	if first.Resolution == nil || first.Resolution.Action != ActRerunChecks || first.Comment != 0 {
		t.Fatalf("first outcome = %+v, want rerun_checks with no comment", first)
	}
	card, _, _ := g.s.Task(context.Background(), "t1")
	if f, err := g.r.tripFacts(context.Background(), card); err != nil || !f.RerunOnHead || f.Checks != "red" {
		t.Fatalf("facts after = %+v, %v; want the checks of red h2 run again", f, err)
	}

	second := g.trip(t, tripCap)
	if second.Resolution != nil || second.CaseID == first.CaseID || second.Comment != 501 || second.Rung != 3 {
		t.Fatalf("second outcome = %+v, want a new case at the human on comment 501", second)
	}
	c := caseByID(t, g.s, second.CaseID)
	if c.Status != CaseAtHuman || c.StateKey != "h2" || c.Follows != first.CaseID || c.CommentID != 501 {
		t.Fatalf("second case = %+v, want h2's at the human following case %d with comment 501", c, first.CaseID)
	}
	steps := caseStepsOf(t, g.s, c.ID)
	if len(steps) != 2 || steps[0].Rung != 0 || steps[0].Outcome != StepHandedUp ||
		steps[0].Found != "rung 0 took this stop with rerun_checks before, and it came back" || steps[1].Rung != 3 || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0 handing up the stop that came back, then the human's", steps)
	}
	if len(g.forge.comments) != 1 || len(g.asks(t)) != 1 {
		t.Fatalf("comments %d, asks %d; want the human face once", len(g.forge.comments), len(g.asks(t)))
	}
}

// Two trips with one key while the case waits at the human: one case,
// whose second step says the stop came again, and one comment.
func TestOpenTripRepeatsAppendAndAskNothingNew(t *testing.T) {
	g := newTripRig(t, "success")
	g.atHead(t, false)
	first := g.trip(t, tripCap)
	again := g.trip(t, tripCap)

	if again != first || first.Comment != 501 || first.Rung != 3 {
		t.Fatalf("outcomes %+v and %+v, want the same case on comment 501 at the human", first, again)
	}
	if cases, _ := g.s.CasesOf(context.Background(), "t1"); len(cases) != 1 {
		t.Fatalf("cases = %+v, want one", cases)
	}
	c := caseByID(t, g.s, first.CaseID)
	var found []string
	for _, st := range caseStepsOf(t, g.s, c.ID) {
		found = append(found, fmt.Sprintf("%d %s %s", st.Rung, st.Outcome, st.Found))
	}
	want := []string{"0 handed_up no resolver fits", "3 in_progress ", "0 handed_up the same stop again: " + c.Problem}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("steps %q, want %q", found, want)
	}
	if len(g.forge.comments) != 1 || len(g.asks(t)) != 1 {
		t.Fatalf("comments %d, asks %d; want one each", len(g.forge.comments), len(g.asks(t)))
	}
}

// A restart's trip waits on the card's live ask with a step and no new
// face. With no live ask, the question is asked again and its words open
// the case the run waits on.
func TestOpenTripHealedFindsTheLiveAsk(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	asked := g.trip(t, AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt")
	healed := g.trip(t, healedReason)
	if healed != asked || len(g.forge.comments) != 1 {
		t.Fatalf("healed %+v after %+v with %d comments; want the same case and comment, no new face", healed, asked, len(g.forge.comments))
	}
	steps := caseStepsOf(t, g.s, asked.CaseID)
	if last := steps[len(steps)-1]; last.Rung != 0 || last.Outcome != StepHandedUp || last.Found != "asked again after a restart" {
		t.Fatalf("last step = %+v, want the restart's", last)
	}

	if err := g.s.WithdrawCase(ctx, asked.CaseID, "moved on"); err != nil {
		t.Fatal(err)
	}
	again := g.trip(t, healedReason)
	c := caseByID(t, g.s, again.CaseID)
	if again.Comment != 502 || c.Kind != CaseOther || c.Status != CaseAtHuman || c.Problem != "Which file?" || c.CommentID != 502 {
		t.Fatalf("outcome %+v, case %+v; want the question asked again on comment 502 and its case", again, c)
	}
}

// The human face's question_asked row names its case, and the case writer
// appends the human's step to that case and takes its comment, opening no
// case of the ask's words.
func TestOpenTripWritesTheFaceWithTheCaseID(t *testing.T) {
	g := newTripRig(t, "success")
	out := g.trip(t, AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt")

	asks := g.asks(t)
	if len(asks) != 1 || asks[0].Detail["case_id"] != float64(out.CaseID) || asks[0].Detail["comment"] != 501.0 {
		t.Fatalf("asks = %+v, want one naming case %d and comment 501", asks, out.CaseID)
	}
	cases, err := g.s.CasesOf(context.Background(), "t1")
	if err != nil || len(cases) != 1 || cases[0].ID != out.CaseID {
		t.Fatalf("cases = %+v, %v; want the trip's alone", cases, err)
	}
	c := cases[0]
	if c.Kind != CaseAgentEscalate || c.Status != CaseAtHuman || c.CommentID != 501 || c.Problem != "Which file?" || c.Facts["source"] != "agent" {
		t.Fatalf("case = %+v, want the agent's ask at the human on comment 501", c)
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) != 2 || steps[1].Rung != 3 || steps[1].Actor != FactoryDecider || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0's, then the human's", steps)
	}
}

// A run from before the cases gate asks through Escalate: the stop's case
// opens at the human by the trip's key, the head, and the face names it.
func TestEscalateOpensTheCaseByTheTripsKey(t *testing.T) {
	g := newTripRig(t, "success")
	g.atHead(t, true)
	id, err := g.r.Escalate(context.Background(), "t1", tripCap, nil)
	if err != nil || id != 501 {
		t.Fatalf("Escalate = %d, %v; want comment 501", id, err)
	}
	cases, _ := g.s.CasesOf(context.Background(), "t1")
	if len(cases) != 1 || cases[0].Kind != CaseSessionCap || cases[0].StateKey != "h2" || cases[0].Status != CaseAtHuman || cases[0].CommentID != 501 {
		t.Fatalf("cases = %+v, want the session cap at h2 at the human on comment 501, no rung 0", cases)
	}
	if asks := g.asks(t); len(asks) != 1 || asks[0].Detail["case_id"] != float64(cases[0].ID) {
		t.Fatalf("asks = %+v, want one naming case %d", asks, cases[0].ID)
	}
}

// A budget floor's case carries the money: 0.30 EUR an opening needs less
// the 0.15 left of 3 EUR with 2.85 spent is 0.15 EUR, which buys one
// session on the card's model.
func TestOpenTripCarriesTheMoneyFact(t *testing.T) {
	g := newTripRig(t, "success")
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET spent_eur = 2.85, model_alias = 'factory/small' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, budgetRefused+": 0.150 EUR of the 3.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR")

	c := caseByID(t, g.s, out.CaseID)
	want := CaseMoney{Kind: "budget", BudgetEUR: 3, SpentEUR: 2.85, NeedEUR: 0.15, Buys: "one session on factory/small"}
	if c.Kind != CaseBudgetFloor || c.Money == nil || *c.Money != want || out.Comment != 501 {
		t.Fatalf("case %+v, money %+v; want the budget floor at the human with %+v", c, c.Money, want)
	}
}

// An ask that names its live case puts that case at the human with the
// ask's comment and the human's step. One that names a case that ended
// opens a case of its words, as an ask that names none does.
func TestAnAskNamingItsCaseTakesThatCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	c, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseSessionCap, StateKey: "h2", Problem: "Card 1410 used its sessions"})
	if err != nil {
		t.Fatal(err)
	}
	ask := map[string]any{"question": capAsk, "ask": capAsk, "comment": int64(27001), "source": "cap", "reason": capReached, "case_id": c.ID}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, ask)
	got := onlyOpenCase(t, s)
	if got.ID != c.ID || got.Status != CaseAtHuman || got.CommentID != 27001 || got.Problem != "Card 1410 used its sessions" {
		t.Fatalf("case = %+v, want case %d at the human on comment 27001", got, c.ID)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Rung != 3 || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want the human's", steps)
	}

	if err := s.WithdrawCase(ctx, c.ID, "moved on"); err != nil {
		t.Fatal(err)
	}
	ask["comment"] = int64(27002)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, ask)
	if again := onlyOpenCase(t, s); again.ID == c.ID || again.Problem != capAsk || again.CommentID != 27002 || again.Follows != c.ID {
		t.Fatalf("case = %+v, want a case of the ask's words following case %d", again, c.ID)
	}
}
