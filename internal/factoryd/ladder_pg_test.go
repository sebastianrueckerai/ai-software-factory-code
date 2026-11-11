package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tripRig is a runner on Postgres with card t1, a large agent card of
// demo-app/demo-app on the direct track, running on issue 12 with a
// budget of 3 EUR, under the policy of the act rig with session floors of
// 0.30 EUR. Forgejo answers the checks of the heads in checks by their
// state, takes comments on issue 12 (ids 501 on, failing the next one
// when failComment is set) and its labels, and knows nothing else. The
// workflows record every signal.
type tripRig struct {
	r       *Runner
	s       *PGStore
	forge   *tripForge
	signals *caseSignals
}

type tripForge struct {
	mu          sync.Mutex
	checks      map[string]string // a head's combined state: success or failure
	comments    []string
	failComment bool
	// files holds a repository file by "ref:path", prCommits the body of
	// pull request 9's commits, and redLog the log of the one failed job of
	// run 7, which the red heads' statuses point at when it is set
	files     map[string]string
	prCommits string
	redLog    string
}

func (f *tripForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const issue = "/api/v1/repos/demo-app/demo-app/issues/12"
		const repo = "/api/v1/repos/demo-app/demo-app"
		switch {
		case strings.HasPrefix(r.URL.Path, repo+"/raw/"):
			body, ok := f.files[r.URL.Query().Get("ref")+":"+strings.TrimPrefix(r.URL.Path, repo+"/raw/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, body)
		case r.URL.Path == repo+"/pulls/9/commits" && r.URL.Query().Get("page") == "1" && f.prCommits != "":
			fmt.Fprint(w, f.prCommits)
		case r.URL.Path == repo+"/pulls/9/commits":
			fmt.Fprint(w, `[]`)
		case r.URL.Path == repo+"/actions/runs":
			fmt.Fprint(w, `{"workflow_runs":[{"id":70,"index_in_repo":7}]}`)
		case r.URL.Path == repo+"/actions/runs/70/jobs":
			fmt.Fprint(w, `[{"id":12,"name":"test","status":"failure","attempt":1}]`)
		case r.URL.Path == repo+"/actions/jobs/12/logs":
			fmt.Fprint(w, f.redLog)
		case r.Method == http.MethodPost && r.URL.Path == issue+"/comments" && f.failComment:
			f.failComment = false
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == issue+"/comments":
			b, _ := io.ReadAll(r.Body)
			f.comments = append(f.comments, string(b))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.comments))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/commits/") && strings.HasSuffix(r.URL.Path, "/status") &&
			f.checks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/commits/"), "/status")] != "":
			state := f.checks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/commits/"), "/status")]
			target := ""
			if state == "failure" && f.redLog != "" {
				target = "/demo-app/demo-app/actions/runs/7/jobs/0"
			}
			fmt.Fprintf(w, `{"state":%q,"statuses":[{"status":%[1]q,"context":"ci / test","target_url":%q}]}`, state, target)
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
	signals, forge := &caseSignals{}, &tripForge{checks: map[string]string{"h2": checks}}
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
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: reason}, tripAttempt{N: 1})
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
	rungsOff(g.r)
	g.atHead(t, false)
	first := g.trip(t, tripCap)
	if first.Resolution == nil || first.Resolution.Action != ActRerunChecks || first.Comment != 0 {
		t.Fatalf("first outcome = %+v, want rerun_checks with no comment", first)
	}
	card, _, _ := g.s.Task(context.Background(), "t1")
	if f, err := g.r.tripFacts(context.Background(), card, "", ""); err != nil || !f.RerunOnHead || f.Checks != "red" {
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
	rungsOff(g.r)
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
// face. With no live ask, the stop the card asked last, the agent's ask,
// opens its own case again and the run waits on it.
func TestOpenTripHealedFindsTheLiveAsk(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rungsOff(g.r)
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
	if again.Comment != 502 || c.Kind != CaseAgentEscalate || c.Status != CaseAtHuman || c.Problem != "Which file?" || c.CommentID != 502 {
		t.Fatalf("outcome %+v, case %+v; want the question asked again on comment 502 as the agent's ask", again, c)
	}
}

// The human face's question_asked row names its case, and the case writer
// appends the human's step to that case and takes its comment, opening no
// case of the ask's words.
func TestOpenTripWritesTheFaceWithTheCaseID(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
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

// Escalate opens its case open and puts it at the human once its face is
// written, as OpenTrip does: a face whose comment fails leaves the case
// open, faceless, for the heal pass to weigh by its age.
func TestEscalateOpensItsCaseBeforeItsFace(t *testing.T) {
	g := newTripRig(t, "success")
	g.forge.failComment = true
	if _, err := g.r.Escalate(context.Background(), "t1", tripCap, nil); err == nil {
		t.Fatal("Escalate = nil; want the comment's failure")
	}
	cases, err := g.s.CasesOf(context.Background(), "t1")
	if err != nil || len(cases) != 1 || cases[0].Kind != CaseSessionCap || cases[0].Status != CaseOpen {
		t.Fatalf("cases = %+v, %v; want the session cap open, its face not written", cases, err)
	}
}

// A budget floor's case carries the money: 0.30 EUR an opening needs less
// what a session may spend, the 0.15 left of 3 EUR with 2.85 spent less
// the 0.40 gate reserve, -0.25, is 0.55 EUR, which buys one session on
// the card's model.
func TestOpenTripCarriesTheMoneyFact(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET spent_eur = 2.85, model_alias = 'factory/small' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, budgetRefused+": 0.150 EUR of the 3.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR")

	c := caseByID(t, g.s, out.CaseID)
	want := CaseMoney{Kind: "budget", BudgetEUR: 3, SpentEUR: 2.85, NeedEUR: 0.55, Buys: "one session on factory/small"}
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

// h1 reached its approval green and approved, then a change request sent
// the card back, and its sessions pushed h2, whose checks are red. The
// session cap's trip on h2 decides on h2: its checks run again, no route
// to the approval on h1's facts.
func TestOpenTripDecidesOnTheTripsHead(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "failure")
	g.forge.checks["h1"] = "success"
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{
		{Kind: EventGateHead, Detail: map[string]any{"gate": GateMerge, "head": "h1"}},
		{Kind: EventModelReview, Detail: map[string]any{"commit": "h1", "round": 1, "reviewed": true, "verdict": "approve"}},
	} {
		e.Repository, e.TaskID, e.IssueNumber, e.Actor, e.Via = "demo-app/demo-app", "t1", 12, FactoryDecider, ViaFactoryd
		if _, err := g.s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	out, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap, Head: "h2"}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolution == nil || out.Resolution.Action != ActRerunChecks {
		t.Fatalf("outcome = %+v, want the checks of h2 run again", out)
	}
	c := caseByID(t, g.s, out.CaseID)
	if c.StateKey != "h2" || c.Facts["head"] != "h2" || c.Facts["checks"] != "red" || c.Facts["review_approved"] != nil {
		t.Fatalf("case key %q, facts %v; want h2's red checks and no approval", c.StateKey, c.Facts)
	}
}

// A head a rerun made and a head a rerun ran on both count as run again:
// the queue ran h2's checks again as h3. A trip on red h3 runs no checks
// again, and the human is asked.
func TestTripFactsCountAHeadARerunMade(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "failure")
	rungsOff(g.r)
	g.forge.checks["h3"] = "failure"
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.r.record(ctx, "t1", "checks_rerun", map[string]any{"commit": "h2", "head": "h3"})
	card, _, _ := g.s.Task(ctx, "t1")
	for head, want := range map[string]bool{"h2": true, "h3": true, "h4": false} {
		if f, err := g.r.tripFacts(ctx, card, head, ""); err != nil || f.RerunOnHead != want || f.Head != head {
			t.Errorf("facts on %s: head %q, RerunOnHead %v, %v; want %v", head, f.Head, f.RerunOnHead, err, want)
		}
	}
	out, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap, Head: "h3"}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	steps := caseStepsOf(t, g.s, out.CaseID)
	if out.Resolution != nil || out.Comment != 501 || len(steps) == 0 || steps[0].Found != "no resolver fits" {
		t.Fatalf("outcome %+v, steps %+v; want the human asked with no resolver fitting", out, steps)
	}
}

// An attempt of the trip retried after its first wrote its step but not
// its face writes the step once; one retried after the face writes
// nothing more and asks nothing new.
func TestOpenTripRetriedWritesItsStepsOnce(t *testing.T) {
	ctx := context.Background()
	steps := func(g tripRig, id int64) []string {
		var out []string
		for _, st := range caseStepsOf(t, g.s, id) {
			out = append(out, fmt.Sprintf("%d %s %s", st.Rung, st.Outcome, st.Found))
		}
		return out
	}
	want := []string{"0 handed_up no resolver fits", "3 in_progress "}
	t.Run("after the step", func(t *testing.T) {
		g := newTripRig(t, "success")
		rungsOff(g.r)
		g.forge.failComment = true
		in := TripInput{TaskID: "t1", Reason: tripCap}
		if _, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 1}); err == nil {
			t.Fatal("the first attempt's face did not fail")
		}
		out, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 2})
		if err != nil || out.Comment != 501 {
			t.Fatalf("second attempt = %+v, %v; want the face on comment 501", out, err)
		}
		if got := steps(g, out.CaseID); !reflect.DeepEqual(got, want) {
			t.Fatalf("steps %q, want %q", got, want)
		}
	})
	t.Run("after the face", func(t *testing.T) {
		g := newTripRig(t, "success")
		rungsOff(g.r)
		in := TripInput{TaskID: "t1", Reason: tripCap}
		first, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 1})
		if err != nil {
			t.Fatal(err)
		}
		again, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 2})
		if err != nil || again != first {
			t.Fatalf("second attempt = %+v, %v; want %+v", again, err, first)
		}
		if got := steps(g, first.CaseID); !reflect.DeepEqual(got, want) || len(g.forge.comments) != 1 {
			t.Fatalf("steps %q, comments %d; want %q and one comment", got, len(g.forge.comments), want)
		}
	})
}

// An ask that names another card's case goes by its words: a case of the
// asking card opens, and the other card's case is left as it was.
func TestAnAskNamingAnotherCardsCaseGoesByItsWords(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	if err := s.CreateTask(ctx, Task{ID: "t-other", Kind: "agent", Status: "waiting-human", Objective: "y", Repository: "sf/r",
		IssueNumber: 1411, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatal(err)
	}
	other, _, err := s.OpenCase(ctx, Case{TaskID: "t-other", Repository: "sf/r", Kind: CaseSessionCap, StateKey: "h2", Problem: "Card 1411 used its sessions"})
	if err != nil {
		t.Fatal(err)
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": capAsk, "ask": capAsk, "comment": int64(27001), "source": "cap", "reason": capReached, "case_id": other.ID})
	mine := onlyOpenCase(t, s)
	if mine.ID == other.ID || mine.Problem != capAsk || mine.CommentID != 27001 {
		t.Fatalf("the asking card's case = %+v, want one of its own words", mine)
	}
	if got := caseByID(t, s, other.ID); got.Status != CaseOpen || got.CommentID != 0 || len(caseStepsOf(t, s, other.ID)) != 0 {
		t.Fatalf("the other card's case = %+v, want it untouched", got)
	}
}

// extendOne is sebastian's extend of one session on t1's case id, carried
// by his comment 77, and what it left: the card's raised sessions and its
// card_extended rows.
func (g tripRig) extendOne(t *testing.T, id int64) (int, []Event) {
	t.Helper()
	if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: id, By: "sebastian", Via: ViaConsole, Rung: 3,
		Post: posting(77, nil), Res: oneSession}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	card, _, err := g.s.Task(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return card.Usage.RaisedSessions, actRig{s: g.s}.rows(t, EventCardExtended)
}

// A run from before the cases gate asks through Escalate and grants its
// own session when the human's comment wakes it: the human's extend on
// that case writes its draw against the pool as a row of rung 3 and
// raises nothing on the card's row. A case a trip under the gate waits on
// carries the trip's step of rung 0, and the extend raises the row there:
// the case the trip opened, and the old run's case a restarted run under
// the gate waits on.
func TestActLeavesTheGrantToARunFromBeforeTheGate(t *testing.T) {
	poolRow := func(t *testing.T, rows []Event) {
		t.Helper()
		if len(rows) != 1 || rows[0].Detail["pool"] != true || rows[0].Detail["rung"] != 3.0 || rows[0].Detail["sessions"] != 1.0 {
			t.Fatalf("card_extended rows = %+v, want one pool draw of 1 session on rung 3", rows)
		}
	}
	t.Run("a run from before the gate", func(t *testing.T) {
		g := newTripRig(t, "success")
		if _, err := g.r.Escalate(context.Background(), "t1", tripCap, nil); err != nil {
			t.Fatal(err)
		}
		c := onlyLiveCase(t, g.s, "t1")
		raised, rows := g.extendOne(t, c.ID)
		poolRow(t, rows)
		if raised != 0 {
			t.Fatalf("raised sessions = %d, want 0: the run grants its own", raised)
		}
	})
	for name, open := range map[string]func(t *testing.T, g tripRig) int64{
		"a trip's case": func(t *testing.T, g tripRig) int64 { return g.trip(t, tripCap).CaseID },
		"the old run's case after a restart": func(t *testing.T, g tripRig) int64 {
			if _, err := g.r.Escalate(context.Background(), "t1", tripCap, nil); err != nil {
				t.Fatal(err)
			}
			return g.trip(t, healedReason).CaseID
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newTripRig(t, "success")
			id := open(t, g)
			if c := onlyLiveCase(t, g.s, "t1"); c.ID != id {
				t.Fatalf("live case %d, the trip waits on %d", c.ID, id)
			}
			raised, rows := g.extendOne(t, id)
			poolRow(t, rows)
			if raised != 1 {
				t.Fatalf("raised sessions = %d, want 1: the run under the gate reads the row", raised)
			}
		})
	}
}

// onlyLiveCase is taskID's one live case.
func onlyLiveCase(t *testing.T, s *PGStore, taskID string) Case {
	t.Helper()
	open, err := s.OpenCasesOf(context.Background(), taskID)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenCasesOf = %+v, %v; want one case", open, err)
	}
	return open[0]
}

// The face's last line says what moves the card on, by the case's kind:
// under the cases gate a reply at a cap or at a budget floor is a continue
// that asks again, so the face names Extend or the Fund form and the
// chat; any other stop takes a reply. A run from before the gate grants
// on a reply, and its face says so at a cap too.
func TestOpenTripsFaceNamesWhatMovesTheCard(t *testing.T) {
	const reply = "Reply on this issue to start a new attempt"
	extend := "Use Extend on the board, or ask the project chat to extend the card, to start a new attempt. A reply alone starts none."
	fund := "Use the Fund form on the board, or ask the project chat to fund the card, to start a new attempt. A reply alone starts none."
	for _, tc := range []struct {
		name, reason, want string
		escalate           bool
	}{
		{"the session cap", tripCap, extend, false},
		{"the agent time cap", timeCapReached + "\nlast: no session produced a result", extend, false},
		{"the budget floor", budgetRefused + ": 0.150 EUR of the 3.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR", fund, false},
		{"an agent's question", AgentAsks + "Which file?\nWhy: two match\nRecommendation: a.txt", reply, false},
		{"a run from before the gate at the session cap", tripCap, reply, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTripRig(t, "success")
			rungsOff(g.r)
			if tc.escalate {
				if _, err := g.r.Escalate(context.Background(), "t1", tc.reason, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				g.trip(t, tc.reason)
			}
			if len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], tc.want) {
				t.Fatalf("faces = %q, want one ending %q", g.forge.comments, tc.want)
			}
		})
	}
}

// A run under the gate that restarts while its card waits on a human, with
// no live case left, asks its stop again as the stop's own case: the
// newest escalation, the session cap's, opens the cap's case with a step of
// rung 0 that says so, and the face names Extend, not a reply. The
// human's extend there raises the card's row, which that run reads.
func TestOpenTripRestartWithNoLiveCaseAsksItsStopAgain(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	rungsOff(g.r)
	if _, err := g.s.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: IntakeEscalation, Author: "factoryd", Body: tripCap,
		CommentID: 400, Question: &HumanQuestion{Ask: "Grant one more session?", Why: "the cap", Info: plainInfo}}); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, healedReason)
	c := caseByID(t, g.s, out.CaseID)
	if c.Kind != CaseSessionCap || c.Status != CaseAtHuman || out.Comment != 501 || c.CommentID != 501 {
		t.Fatalf("outcome %+v, case %+v; want the session cap's case at the human on comment 501", out, c)
	}
	if !slices.ContainsFunc(caseStepsOf(t, g.s, c.ID), func(s CaseStep) bool { return s.Rung == 0 && s.Found == "asked again after a restart" }) {
		t.Fatalf("steps = %+v, want the restart's step of rung 0", caseStepsOf(t, g.s, c.ID))
	}
	if extend := "Use Extend on the board, or ask the project chat to extend the card"; len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], extend) {
		t.Fatalf("faces = %q, want one naming Extend", g.forge.comments)
	}
	if raised, _ := g.extendOne(t, c.ID); raised != 1 {
		t.Fatalf("raised sessions = %d, want 1: the run under the gate reads the row", raised)
	}
}

// A restart that finds the card's live ask with no face yet, as a trip
// whose comment failed left it open, gives that case its face rather than
// waiting on a case nobody was asked.
func TestOpenTripRestartFacesALiveAskWithNone(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	g.forge.failComment = true
	if _, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: tripCap}, tripAttempt{N: 1}); err == nil {
		t.Fatal("OpenTrip = nil; want the comment's failure")
	}
	open := onlyLiveCase(t, g.s, "t1")
	out := g.trip(t, healedReason)
	if c := caseByID(t, g.s, open.ID); out.CaseID != open.ID || out.Comment != 501 || c.Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("outcome %+v, case %+v, faces %d; want case %d at the human on comment 501", out, c, len(g.forge.comments), open.ID)
	}
}

// The face a restart gives a live ask with none yet ends on that case's
// own way out: the agent's ask left open by a failed comment takes a
// reply, though the card's newest escalation is the merge queue's.
func TestOpenTripRestartFacesALiveAskByItsOwnReason(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	ctx := context.Background()
	g.forge.failComment = true
	if _, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: AgentAsks + "Which file?\nWhy: two match\nRecommendation: a.txt"}, tripAttempt{N: 1}); err == nil {
		t.Fatal("OpenTrip = nil; want the comment's failure")
	}
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: IntakeEscalation, Author: "factoryd", Body: leftQueue + ": the job is red",
		CommentID: 400, Question: &HumanQuestion{Ask: "Put it back in the queue?", Why: "red", Info: plainInfo}}); err != nil {
		t.Fatal(err)
	}
	g.trip(t, healedReason)
	const reply = "Reply on this issue to start a new attempt with your comment as guidance"
	if len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], reply) {
		t.Fatalf("faces = %q, want one ending %q", g.forge.comments, reply)
	}
}

const tripChecksWait = gateStopped + "WaitChecks (attempt 1) past its bound of 50m: no check reported in the bound."

// A checks-wait stop whose checks are all green, #1652's shape, with the
// head's review approving it too: rung 0 resolves it with wait_checks (not
// route_approval) through Act, no comment, and the run hears of it. The
// same stop on the same head, once the wait ran, goes to the human.
func TestOpenTripWaitsOnGreenChecksOnceThenAsks(t *testing.T) {
	g := newTripRig(t, "success")
	rungsOff(g.r)
	g.atHead(t, true)
	first := g.trip(t, tripChecksWait)
	if first.Resolution == nil || first.Resolution.Action != ActWaitChecks || first.Comment != 0 || first.Rung != 0 || first.CaseID == 0 {
		t.Fatalf("first outcome = %+v, want wait_checks on rung 0 with no comment", first)
	}
	c := caseByID(t, g.s, first.CaseID)
	if c.Kind != CaseGateStop || c.Facts["gate"] != ActivityWaitChecks || c.Status != CaseResolved ||
		!reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWaitChecks}) {
		t.Fatalf("case = %+v, resolution %+v; want the checks-wait stop resolved by factoryd on rung 0 with wait_checks", c, c.Resolution)
	}
	if got := g.signals.got; len(got) != 1 || got[0].signal != SignalCaseResolved || got[0].sig.Case == nil || got[0].sig.Case.CaseID != c.ID {
		t.Fatalf("signals = %+v, want case_resolved of case %d", got, c.ID)
	}
	card, _, _ := g.s.Task(context.Background(), "t1")
	if f, err := g.r.tripFacts(context.Background(), card, "", ""); err != nil || !f.WaitedOnHead {
		t.Fatalf("facts after = %+v, %v; want h2 waited on", f, err)
	}
	second := g.trip(t, tripChecksWait)
	if second.Resolution != nil || second.CaseID == first.CaseID || second.Rung != 3 || second.Comment == 0 {
		t.Fatalf("second outcome = %+v, want a new case at the human", second)
	}
}

// A restart asks the question again with no face of its own, as production
// writes it: the newest ask of the case is a whole face all the same, and
// the case's Why stays on its own reason.
func TestARepeatedAskCarriesTheFaceItRepeats(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	reason := capReached + "\nlast: no session produced a result\nRecommendation: a.txt, it is first"
	if _, err := g.r.Escalate(ctx, "t1", reason, []string{"a.txt", "b.txt"}); err != nil {
		t.Fatal(err)
	}
	// a proposal lands on the ask before the restart asks again
	pre := CaseAction{Action: ActExtend, Args: map[string]any{"sessions": 1}}.preselected()
	if cs, _ := g.s.OpenCasesOf(ctx, "t1"); len(cs) == 1 {
		if err := g.s.SetQuestionProposal(ctx, "t1", cs[0].ID, false, "extend: one more session fixes it", "the factory agent", pre); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.r.Escalate(ctx, "t1", healedReason, nil); err != nil {
		t.Fatal(err)
	}
	asks := g.asks(t)
	if len(asks) != 2 || asks[1].Detail["reason"] != healedReason {
		t.Fatalf("asks = %+v, want the ask and its restart's", asks)
	}
	d, err := g.s.CaseDesk(ctx, "demo-app/demo-app")
	if err != nil || len(d.Cases) != 1 {
		t.Fatalf("desk = %+v, %v", d, err)
	}
	c := d.Cases[0]
	face := CaseFace(c, nil, FactsOf(c), d.Asks[c.ID], nil)
	first := CaseFace(c, nil, FactsOf(c), CaseAsk{Detail: asks[0].Detail}, nil)
	if face.Info == "" || len(face.Choices) == 0 || face.Info != first.Info || len(face.Choices) != len(first.Choices) {
		t.Errorf("face after the restart = %+v, want the first ask's %+v", face, first)
	}
	if face.Recommendation != "extend: one more session fixes it" || face.Source != "the factory agent" || face.Preselected != ActExtend {
		t.Errorf("face after the restart = %+v, want the proposal carried over: recommendation, writer and preselected action", face)
	}
	if !strings.Contains(face.Why, "session") || strings.Contains(face.Why, "restarted") {
		t.Errorf("Why = %q, want the case's own reason", face.Why)
	}
}

// A human card's ask and a plan question's reach the face with their info
// and choices.
func TestHumanCardAndPlanQuestionAsksReachTheFace(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	task := Task{ID: "t1", Kind: "human", Repository: "demo-app/demo-app", IssueNumber: 12}
	q := HumanQuestion{Ask: "Which store keeps the snapshots?", Why: "Two fit.", Info: "Postgres is shared.", Recommendation: "Postgres", Source: "the agent",
		Choices: []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}, {Value: "say:SQLite", Label: "SQLite"}}}
	if err := recordAsk(ctx, g.s, g.r.timeline, task, q, "factoryd", "chat", ViaFactoryd, "", nil); err != nil {
		t.Fatal(err)
	}
	d, err := g.s.CaseDesk(ctx, "demo-app/demo-app")
	if err != nil || len(d.Cases) != 1 {
		t.Fatalf("desk = %+v, %v", d, err)
	}
	c := d.Cases[0]
	face := CaseFace(c, nil, FactsOf(c), d.Asks[c.ID], nil)
	if face.Info != "Postgres is shared." || len(face.Choices) != 2 || face.Recommendation != "Postgres" || face.Source != "the agent" {
		t.Errorf("the human card's face = %+v", face)
	}
	rev, err := g.s.AddPlanRevision(ctx, "t1", json.RawMessage(`{"summary": "one"}`), "s1")
	if err != nil {
		t.Fatal(err)
	}
	planTask, _, _ := g.r.task(ctx, "t1")
	if err := g.r.askQuestions(ctx, planTask, rev, []PlanQuestion{{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}, Recommended: "Postgres", Grounds: "it is shared"}}); err != nil {
		t.Fatal(err)
	}
	d, _ = g.s.CaseDesk(ctx, "demo-app/demo-app")
	var plan Case
	for _, k := range d.Cases {
		if k.Kind == CasePlanQuestion {
			plan = k
		}
	}
	if plan.ID == 0 {
		t.Fatalf("no plan question case in %+v", d.Cases)
	}
	pf := CaseFace(plan, nil, FactsOf(plan), d.Asks[plan.ID], nil)
	if len(pf.Choices) != 2 || pf.Recommendation != "Postgres: it is shared" || pf.Source != "the planner" {
		t.Errorf("the plan question's face = %+v", pf)
	}
}

// The factory agent's statement for a human card's case, and for a plan
// question's, offers that ask's own choices and never the action table of
// a card.
func TestRungTwoStatementOffersTheAsksOwnChoices(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.task(ctx, "h1")
	q := HumanQuestion{Ask: "Which store?", Why: "Two fit.", Info: "Postgres is shared.", Choices: []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}, {Value: "say:SQLite", Label: "SQLite"}}}
	if err := recordAsk(ctx, g.s, g.r.timeline, h, q, "factoryd", "agent", ViaChat, "", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "h1")
	if len(cases) != 1 || cases[0].Kind != CaseHumanCard {
		t.Fatalf("cases = %+v; want the chat-escalated card's one human card case", cases)
	}
	text, err := g.r.RungTwoStatement(ctx, cases[0].ID)
	if err != nil || !strings.HasPrefix(text, "Which store?\n") || !strings.Contains(text, "The ask's choices:\n- say:Postgres: Postgres\n- say:SQLite: SQLite\n") ||
		strings.Contains(text, "- resume") || strings.Contains(text, "- handover") {
		t.Errorf("the human card's statement = %q, %v; want its problem and the ask's own two choices, no action table", text, err)
	}

	g.r.timeline.RecordRow(ctx, "t1", EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"question": "Which database?", "revision": 1, "index": 1,
		"choices": []string{"Postgres", "SQLite", "MySQL"}, "offers": []QuestionChoice{{Value: "Postgres", Label: "Postgres"}, {Value: "SQLite", Label: "SQLite"}, {Value: "MySQL", Label: "MySQL"}}})
	plan, _ := g.s.OpenCasesOf(ctx, "t1")
	var pq Case
	for _, c := range plan {
		if c.Kind == CasePlanQuestion {
			pq = c
		}
	}
	ptext, err := g.r.RungTwoStatement(ctx, pq.ID)
	if err != nil || !strings.HasPrefix(ptext, "Which database?\n") || !strings.Contains(ptext, "The ask's choices:\n- Postgres: Postgres\n- SQLite: SQLite\n- MySQL: MySQL\n") || strings.Contains(ptext, "- resume") {
		t.Errorf("the plan question's statement = %q, %v", ptext, err)
	}
}

// A rung's recommendation is empty without grounds and names its writer.
func TestRecommendationNeedsGroundsAndNamesItsWriter(t *testing.T) {
	g := newTripRig(t, "success")
	if got := g.r.recommendation(Task{}, &handUp{Action: ActClose, Grounds: "  "}); got != "" {
		t.Errorf("a recommendation with no grounds = %q, want none", got)
	}
	if got := g.r.recommendation(Task{}, &handUp{Action: ActClose, Grounds: "done"}); got != "close: done" {
		t.Errorf("recommendation = %q", got)
	}
	if recommendedBy(&handUp{ActionRung: 2}) != "the factory agent" || recommendedBy(&handUp{ActionRung: 1}) != "the check-in" {
		t.Error("the writers' names are not the factory agent's and the check-in's")
	}
}

// A statement lists the choices of its own case's ask: a card with two live
// human asks gives each case the choices it asked.
func TestRungTwoStatementTakesItsOwnCasesAsk(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.task(ctx, "h1")
	for _, q := range []HumanQuestion{
		{Ask: "Which store?", Why: "w", Info: "i", Choices: []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}}},
		{Ask: "Which port?", Why: "w", Info: "i", Choices: []QuestionChoice{{Value: "say:8080", Label: "8080"}}},
	} {
		if err := recordAsk(ctx, g.s, g.r.timeline, h, q, "factoryd", "webhook", ViaFactoryd, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	cases, _ := g.s.OpenCasesOf(ctx, "h1")
	if len(cases) != 2 {
		t.Fatalf("cases = %+v; want two", cases)
	}
	for i, want := range []string{"- say:Postgres: Postgres\n", "- say:8080: 8080\n"} {
		text, err := g.r.RungTwoStatement(ctx, cases[i].ID)
		other := []string{"say:8080", "say:Postgres"}[i]
		if err != nil || !strings.Contains(text, want) || strings.Contains(text, other) {
			t.Errorf("case %d's statement = %q, %v; want its own choice %q and not %q", cases[i].ID, text, err, want, other)
		}
	}
}

// A trip on a chat-escalated human card opens a human card case, as the
// ask's own event does: one helper decides it for both.
func TestTripCaseMakesAChatEscalatedHumanCardAHumanCard(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.task(ctx, "h1")
	c, _, err := g.r.tripCase(ctx, h, CaseFacts{}, TripInput{TaskID: "h1", Reason: AgentAsks + "Which store?"})
	if err != nil || c.Kind != CaseHumanCard {
		t.Fatalf("trip case = %+v, %v; want a human card case", c, err)
	}
	agent := Task{ID: "a1", Kind: "agent"}
	if humanCardKind(CaseAgentEscalate, agent) != CaseAgentEscalate || humanCardKind(CaseMonthCap, h) != CaseMonthCap || humanCardKind(CaseOther, h) != CaseHumanCard {
		t.Error("humanCardKind turns an agent card's ask, or a specialised kind, into a human card")
	}
}
