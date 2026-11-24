package factoryd

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// outsidePlanRig is newRetryRig with t-a and t-b outside any plan, its
// fixture policy the current one, so every fact of a card reads.
func outsidePlanRig(t *testing.T) (*Runner, *fakeStore, *planForgejo) {
	t.Helper()
	r, store, _, fj, _ := newRetryRig(t)
	for _, id := range []string{"t-a", "t-b"} {
		c := store.tasks[id]
		c.Source, c.Parent, c.PlanKey = "webhook", "", ""
		store.tasks[id] = c
	}
	v := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return v }
	return r, store, fj
}

// Outside a plan rung 0 files a failed dependency again, once: card B's
// trip on A runs RetryCard and its case resolves with retry. When A's
// replacement fails too, B's trip keys on the replacement, rung 0 retries
// no retry, rung 2 hands it on, and the human is asked.
func TestOpenTripRetriesAFailedDependencyOnce(t *testing.T) {
	ctx := context.Background()
	r, store, fj := outsidePlanRig(t)
	rung1Off(r)
	first, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: DepFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	retry := Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}
	if first.Resolution == nil || !reflect.DeepEqual(*first.Resolution, retry) || first.Comment != 0 || first.Rung != 0 {
		t.Fatalf("first outcome = %+v, want rung 0's retry of t-a", first)
	}
	c, _ := store.CaseByID(ctx, first.CaseID)
	if c.Kind != CaseDependencyFailed || c.StateKey != "t-a" || c.Status != CaseResolved ||
		c.Resolution == nil || c.Resolution.By != FactoryDecider || c.Resolution.Rung != 0 || c.Resolution.Action != ActRetry {
		t.Fatalf("case = %+v, resolution %+v; want t-a's failure resolved by factoryd on rung 0 with retry", c, c.Resolution)
	}
	again := RetryID("t-a")
	n, ok := store.tasks[again]
	if !ok || n.Status != "queued" || !slices.Equal(store.deps["t-b"], []string{again}) || fj.issues[5].state != "closed" {
		t.Fatalf("replacement %+v (found %v), B's deps %v, #5 %s; want B waiting on the queued replacement and #5 closed",
			n, ok, store.deps["t-b"], fj.issues[5].state)
	}

	n.Status = "failed"
	store.tasks[again] = n
	second, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: DepFailedReason(Dependency{TaskID: again, Issue: n.IssueNumber, Status: "failed"})}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	second = rung2Faces(t, r, "t-b", second)
	if second.Resolution != nil || second.Comment == 0 || second.Rung != 3 {
		t.Fatalf("second outcome = %+v, want the human asked", second)
	}
	c, _ = store.CaseByID(ctx, second.CaseID)
	steps, _ := store.CaseSteps(ctx, c.ID)
	if c.StateKey != again || c.Status != CaseAtHuman || len(steps) != 3 || steps[0].Found != "no resolver fits" || steps[0].Outcome != StepHandedUp ||
		steps[1].Rung != 2 || steps[2].Rung != 3 || steps[2].CommentID != second.Comment {
		t.Fatalf("case %+v, steps %+v; want the replacement's failure at the human, rung 0 finding no resolver, rung 2's hand-up, then the face at comment %d", c, steps, second.Comment)
	}
	if _, ok := store.tasks[RetryID(again)]; ok {
		t.Fatal("the replacement was retried in turn")
	}
}

// Rung 0's rows on a case's kind and facts, the first that fires winning:
// a green approved head goes to its approval, a red one's checks run once,
// and a failed dependency is retried outside a plan and sent to its
// planner inside one, unless it is a retry itself. The cap row reads the
// cap itself: the retake tests hold it.
func TestRungZeroRowsFireOnTheirFacts(t *testing.T) {
	room := CaseFacts{SessionCap: 3, AgentTimeCapS: 3600}
	head := func(checks string, approved bool) CaseFacts {
		f := room
		f.Head, f.PR, f.PROpen, f.Checks, f.ReviewApproved = "h2", 9, true, checks, approved
		return f
	}
	failed := func(plan, retryOf string) CaseFacts {
		f := room
		f.Plan, f.Dependencies = plan, []DepFact{{TaskID: "t-a", Issue: 5, Status: "failed", RetryOf: retryOf}}
		return f
	}
	checksWait := func(checks string, waited bool) CaseFacts {
		f := head(checks, false)
		f.Kind, f.Gate, f.WaitedOnHead = CaseGateStop, ActivityWaitChecks, waited
		return f
	}
	replaced := failed("", "")
	replaced.Dependencies[0].Replaced = RetryID("t-a")
	for _, tc := range []struct {
		name string
		kind CaseKind
		f    CaseFacts
		want *Resolution
	}{
		{"green and approved", CaseSessionCap, head("green", true), &Resolution{Action: ActRouteApproval}},
		{"green and approved, no longer merging into main", CaseSessionCap, func() CaseFacts { f := head("green", true); f.MergeConflict = "conflict"; return f }(), nil},
		{"green, not approved", CaseGateStop, head("green", false), nil},
		{"red", CaseBudgetFloor, head("red", false), &Resolution{Action: ActRerunChecks}},
		{"red, run again", CaseCheckinShare, func() CaseFacts { f := head("red", false); f.RerunOnHead = true; return f }(), nil},
		{"a checks wait's stop with every check green", CaseGateStop, checksWait("green", false), &Resolution{Action: ActWaitChecks}},
		{"a checks wait's stop, waited on this head already", CaseGateStop, checksWait("green", true), nil},
		{"a checks wait's stop with red checks", CaseGateStop, checksWait("red", false), &Resolution{Action: ActRerunChecks}},
		{"a checks wait's stop with checks pending", CaseGateStop, checksWait("pending", false), nil},
		{"green at a stop of no head", CaseAgentEscalate, head("green", true), nil},
		{"a failed dependency", CaseDependencyFailed, failed("", ""), &Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}},
		{"a failed retry", CaseDependencyFailed, failed("", "t-old"), nil},
		// a retry filed its replacement already: rung 0's retry names it (Act)
		{"a dependency retried already", CaseDependencyFailed, replaced, &Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}},
		{"a failed dependency in a plan", CaseDependencyFailed, failed("t-plan", ""), &Resolution{Action: ActReplan, Args: map[string]any{"task_id": "t-a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{Kind: tc.kind, StateKey: tripKey(tc.kind, "the stop", tc.f.Head, tc.f)}
			if tc.kind == CaseDependencyFailed {
				c.StateKey = "t-a" // the dependency the case opened on, replaced or not
			}
			var got *Resolution
			for _, row := range rung0 {
				if row.Fire == nil || !row.tries(c) {
					continue // a row that holds its case resolves nothing, and the kind table names the rows a kind tries
				}
				if res, err := row.Fire(context.Background(), nil, c, tc.f); err != nil || res != nil {
					got = res
					break
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rung 0 took %+v, want %+v", got, tc.want)
			}
		})
	}
}

// route_approval's words for the human whose case it takes say what its
// rule checked: green checks, the newest review's approval and a pull
// request that still merges into main, and for a policy class, which goes
// to the approval unreviewed, green checks and the merge alone.
func TestRouteApprovalSaysWhatItChecked(t *testing.T) {
	i := slices.IndexFunc(rung0, func(r resolver) bool { return r.Name == "route_approval" })
	for kind, want := range map[CaseKind]string{
		CaseSessionCap:  "every check on head 1a2b3c4 is green, its newest review approved it and it still merges into main, so the factory sent it to the approval",
		CasePolicyClass: "every check on head 1a2b3c4 is green and it still merges into main, so the factory sent it to the approval",
	} {
		f := CaseFacts{Kind: kind, SessionCap: 3, AgentTimeCapS: 3600, Head: "1a2b3c4d5e6f", PR: 9, PROpen: true, Checks: "green", ReviewApproved: kind != CasePolicyClass}
		res, err := rung0[i].Fire(context.Background(), nil, Case{Kind: kind}, f)
		if err != nil || res == nil {
			t.Fatalf("%s: route_approval took %+v (%v) on %+v, want it to fire", kind, res, err, f)
		}
		if got := rung0[i].Took(Case{Kind: kind}, f, *res); got != want {
			t.Errorf("%s: words %q, want %q", kind, got, want)
		}
	}
}

// A trip's state key: the head for a stop at it, the dependency's id for a
// dependency's, else the first line of the stop's reason.
func TestTripKeyByKind(t *testing.T) {
	f := CaseFacts{Dependencies: []DepFact{{TaskID: "t-x", Status: "running"}, {TaskID: "t-c", Status: "closed"}, {TaskID: "t-r", Status: "failed", Replaced: "t-r2"},
		{TaskID: "t-a", Status: "failed"}}}
	for _, tc := range []struct {
		kind CaseKind
		head string
		want string
	}{
		{CaseSessionCap, "h2", "h2"},
		{CaseGateStop, "h2", "h2"},
		{CaseSessionCap, "", caseStateKey(CaseSessionCap, "the stop", "")},
		{CaseAgentEscalate, "h2", caseStateKey(CaseAgentEscalate, "the stop", "")},
		{CaseDependencyFailed, "h2", "t-a"},
		{CaseDependencyClosed, "h2", "t-c"},
	} {
		if got := tripKey(tc.kind, "the stop", tc.head, f); got != tc.want {
			t.Errorf("tripKey(%s, %q) = %q, want %q", tc.kind, tc.head, got, tc.want)
		}
	}
}

// A trip names the case's own question as its problem: the agent's
// question tripped in the card's last session is the question, though the
// human's face is the session cap's until a session can run.
func TestATripNamesTheQuestionNotItsCap(t *testing.T) {
	card := Task{ID: "t1", Kind: "agent", IssueNumber: 4}
	f := CaseFacts{Attempts: 3, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, SessionLeastS: 300, BudgetEUR: 2, LeftEUR: 1.5, FloorEUR: 0.3}
	c, err := TripCase(card, f, TripInput{TaskID: card.ID, Reason: AgentAsks + "Which store keeps the snapshots?\nWhy: two fit\nRecommendation: SQLite"}, func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != CaseAgentEscalate || c.Problem != "Which store keeps the snapshots?" {
		t.Fatalf("the trip's case is %s with the problem %q, want the agent's question", c.Kind, c.Problem)
	}
	if face := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: card}); face.Ask != "The card used all 3 sessions. How should it go on?" {
		t.Fatalf("the face asks %q, want the session cap's ask", face.Ask)
	}
}

// A trip's key is the stop's, never its copy: a card out of money on one
// head keys one case whether or not review findings change its ask, and
// another head keys another.
func TestATripKeyOutlivesACopyChange(t *testing.T) {
	card := Task{ID: "t1", Kind: "agent", IssueNumber: 4}
	trip := func(head string, f CaseFacts) Case {
		t.Helper()
		f.Head, f.BudgetEUR, f.SpentEUR, f.FloorEUR, f.CeilingEUR = head, 4, 4, 2, 18
		c, err := TripCase(card, f, TripInput{TaskID: card.ID, Head: head, Reason: BudgetRefused + ": the card is out of money"}, func(s string) string { return s })
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := trip("h1", CaseFacts{})
	id := FindingID("secrets at rig_test.go:71")
	after := trip("h1", CaseFacts{Findings: []string{id}, ReviewFindings: []string{id}, FindingTexts: map[string]string{id: "secrets at rig_test.go:71"}})
	if before.Kind != CaseBudgetFloor || after.StateKey != before.StateKey || after.Problem == before.Problem {
		t.Fatalf("before %q keyed %q, after %q keyed %q; want the same key and another ask", before.Problem, before.StateKey, after.Problem, after.StateKey)
	}
	if other := trip("h2", CaseFacts{}); other.StateKey == before.StateKey {
		t.Fatalf("another head keys %q, the same case", other.StateKey)
	}
}

// A stop the review keeps refusing offers its exits: the deadlock and a
// gate stop whose review raised concerns each offer more sessions, the
// overrule, a fresh card and the end; a plain gate stop offers a check of
// the work instead.
func TestAReviewDeadlockOffersTheExits(t *testing.T) {
	f := CaseFacts{Attempts: 1, SessionCap: 3, AgentTimeCapS: 3600, BudgetEUR: 5, SpentEUR: 1, LeftEUR: 4, FloorEUR: 0.3, CeilingEUR: 7, PROpen: true, Head: "c1", Checks: "green",
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}, PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}
	keys := func(c Case) []string {
		var out []string
		for _, a := range FaceOf(c, Facts{CaseFacts: f, Card: Task{ID: "t1", IssueNumber: 3}}).Answers {
			out = append(out, a.Key())
		}
		return out
	}
	exits := []string{ActExtend, ActReverify + "/" + variantOverrule, ActSuccessor, ActClose}
	for name, c := range map[string]Case{
		"deadlock": {Kind: CaseFindingsRepeated},
		"concerns": {Kind: CaseGateStop, Facts: map[string]any{"reason": "check-in: gate: the checks need a hand\n- " + reviewConcernsMark}},
	} {
		if got := keys(c); !slices.Equal(got, exits) {
			t.Errorf("%s: answers %v, want the exits %v", name, got, exits)
		}
	}
	if got, want := keys(Case{Kind: CaseGateStop, Facts: map[string]any{"reason": "check-in: gate: the checks need a hand"}}), []string{ActReverify, ActContinue, ActReplan, ActClose}; !slices.Equal(got, want) {
		t.Errorf("a plain gate stop: answers %v, want %v", got, want)
	}
}

// A moment's hand_up is a hand-up, not a recommendation: a face whose
// newest rung step is one preselects nothing, while a rung's action it
// handed up is the face's suggestion by that rung.
func TestAMomentsHandUpRecommendsNothing(t *testing.T) {
	c := Case{ID: 1, Kind: CaseOther, Problem: "p"}
	same := func(s string) string { return s }
	moment := []CaseStep{
		{Rung: 2, Outcome: StepHandedUp, Decision: ActClose, Grounds: "g"},
		{Rung: 1, Outcome: StepHandedUp, Decision: RungHandUp, Grounds: "a goal conflict"},
	}
	if row := FaceRow(c, Task{ID: "t1"}, CaseFacts{}, "", HandedUpBy(moment, 1, ""), same, same); row["preselected"] != nil || row["recommended_by"] != nil || row["recommendation"] != nil {
		t.Fatalf("a moment's hand-up preselects %v by %v (%v), want no suggestion", row["preselected"], row["recommended_by"], row["recommendation"])
	}
	rung := FaceRow(c, Task{ID: "t1"}, CaseFacts{}, "", HandedUpBy(moment[:1], 1, ""), same, same)
	if p, _ := rung["preselected"].(map[string]any); p["action"] != ActClose || rung["recommended_by"] != "the factory agent" || rung["recommendation"] != "g" {
		t.Fatalf("a rung's hand-up = %v by %v (%v), want close by the factory agent", rung["preselected"], rung["recommended_by"], rung["recommendation"])
	}
}

// A moment's own hand-up — a stored step with decision hand_up — is the
// hand-up whose reason is what the moment found: no action is recommended
// over it, and the rung that wrote it names the rung of the climb.
func TestHandedUpByReadsAMomentsHandUpStepAsTheHandUp(t *testing.T) {
	steps := []CaseStep{
		{Rung: 0, Outcome: StepHandedUp, Found: "no resolver fits"},
		{Rung: 1, Outcome: StepHandedUp, Decision: RungHandUp, Grounds: "a goal conflict: the objective contradicts the review"},
	}
	up := HandedUpBy(steps, 0, "")
	if up == nil || up.Action != "" || up.Grounds != "" || up.Found != "a goal conflict: the objective contradicts the review" || up.Rung != 1 {
		t.Fatalf("hand-up = %+v, want the moment's grounds found, no action, rung 1", up)
	}
}

// A row whose action fails part way hands the case up with why: the close
// of the failed card's issue fails after the replacement is filed, rung 2
// hands it on, and the human is asked.
func TestOpenTripHandsUpARowThatFails(t *testing.T) {
	ctx := context.Background()
	r, store, fj := outsidePlanRig(t)
	rung1Off(r)
	fj.failCall = "PATCH issues/5"
	out, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: DepFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	out = rung2Faces(t, r, "t-b", out)
	c, _ := store.CaseByID(ctx, out.CaseID)
	steps, _ := store.CaseSteps(ctx, c.ID)
	if out.Resolution != nil || out.Comment == 0 || c.Status != CaseAtHuman || len(steps) != 3 ||
		!strings.HasPrefix(steps[0].Found, "retry_dependency failed: ") || steps[0].Outcome != StepHandedUp || steps[1].Rung != 2 || steps[2].Rung != 3 || steps[2].CommentID != out.Comment {
		t.Fatalf("outcome %+v, case %+v, steps %+v; want the retry's failure handed up, then the face the human is asked on", out, c, steps)
	}
}

// A trip whose first attempt rung 0 resolved, retried before the run
// heard, takes that resolution: the same case, no second retry, no ask.
func TestOpenTripRetriedAfterRungZeroCarriesItsResolution(t *testing.T) {
	ctx := context.Background()
	r, store, _ := outsidePlanRig(t)
	in := TripInput{TaskID: "t-b", Reason: DepFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}
	first, err := r.OpenTrip(ctx, in, tripAttempt{ID: "run/7", N: 1})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.OpenTrip(ctx, in, tripAttempt{ID: "run/7", N: 2})
	retry := &Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}
	if err != nil || !reflect.DeepEqual(again, TripOutcome{CaseID: first.CaseID, Resolution: retry}) {
		t.Fatalf("second attempt = %+v, %v; want case %d's retry of t-a", again, err, first.CaseID)
	}
	if cases, _ := store.CasesOf(ctx, "t-b"); len(cases) != 1 || cases[0].Status != CaseResolved {
		t.Fatalf("cases = %+v, want the first attempt's resolved alone", cases)
	}
	if _, ok := store.tasks[RetryID(RetryID("t-a"))]; ok {
		t.Fatal("the replacement was retried in turn")
	}
}

// A session cap on pull request #9 at h2, green and approved: with every
// fact read rung 0 routes it to the approval. When the timeline cannot be
// read, rung 0 takes nothing, its step names the read that failed, rung 2
// hands it on, and the human is asked.
func TestRungZeroTakesNothingOnFactsReadPartly(t *testing.T) {
	var posted atomic.Int32
	trip := func(t *testing.T, unread bool) (factsRig, TripOutcome) {
		g := newFactsRig(t)
		rung1Off(g.r)
		forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/demo-app/demo-app/commits/h2/status":
				fmt.Fprint(w, `{"state":"success","statuses":[{"status":"success","context":"ci / test"}]}`)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/demo-app/demo-app/issues/12/comments":
				posted.Add(1)
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"id": 501}`)
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/factory-agent/demo-app/branches/factory/t1":
				http.NotFound(w, r)
			default:
				fmt.Fprint(w, `[]`)
			}
		}))
		t.Cleanup(forge.Close)
		g.r.forgejo = &Forgejo{BaseURL: forge.URL, BotUser: "factory-agent", BotPassword: "pw"}
		g.event("t1", factsAt(9, 40), EventGateHead, map[string]any{"head": "h2"})
		g.event("t1", factsAt(9, 45), EventModelReview, map[string]any{"commit": "h2", "round": 1, "reviewed": true, "verdict": "approve"})
		if unread {
			g.r.objects = unreadable{ObjectStore: g.r.objects, key: "sessions/t1/timeline.jsonl"}
		}
		out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: CapReached + "\nlast: no session produced a result", Head: "h2"}, tripAttempt{N: 1})
		if err != nil {
			t.Fatal(err)
		}
		return g, out
	}
	t.Run("every fact read", func(t *testing.T) {
		_, out := trip(t, false)
		if out.Resolution == nil || out.Resolution.Action != ActRouteApproval || out.Comment != 0 {
			t.Fatalf("outcome = %+v, want rung 0's route to the approval", out)
		}
	})
	t.Run("the timeline unread", func(t *testing.T) {
		g, out := trip(t, true)
		if out.Resolution != nil || out.Rung != 2 {
			t.Fatalf("outcome = %+v, want rung 2 holding the case", out)
		}
		if out = rung2Faces(t, g.r, "t1", out); out.Rung != 3 || posted.Load() != 1 {
			t.Fatalf("after rung 2's hand-up: outcome %+v, %d comments; want the human asked on one comment", out, posted.Load())
		}
		c, _ := g.store.CaseByID(context.Background(), out.CaseID)
		steps, _ := g.store.CaseSteps(context.Background(), out.CaseID)
		if c.Status != CaseAtHuman || len(steps) != 2 || steps[0].Found != "facts read partly: timeline, so rung 0 took nothing" || steps[0].Outcome != StepHandedUp ||
			steps[1].Rung != 2 {
			t.Fatalf("case %+v, steps %+v; want it at the human, rung 0 handing up on the unread timeline, then rung 2", c, steps)
		}
	})
}

// A cap's ask counts the review's and the audit's findings standing on
// the head, names the gate that raised them all, never names one, and
// counts no finding the checks alone raised.
func TestCapAskCountsTheReviewAndAuditFindings(t *testing.T) {
	f := CaseFacts{Kind: CaseSessionCap, SessionCap: 3, Attempts: 3, Head: "h2", Findings: []string{"r1", "a1", "c1"},
		FindingTexts:   map[string]string{"r1": "the review's finding", "a1": "the audit's finding", "c1": "the checks' finding"},
		ReviewFindings: []string{"r1"}, AuditFindings: []string{"a1"}}
	c := Case{Kind: CaseSessionCap}
	if got := FaceOf(c, Facts{CaseFacts: f}).Ask; got != "The card used all 3 sessions with 2 findings open. How should it go on?" {
		t.Fatalf("ask = %q", got)
	}
	audit := f
	audit.ReviewFindings, audit.AuditFindings = nil, []string{"r1", "a1"}
	if got := FaceOf(c, Facts{CaseFacts: audit}).Ask; got != "The card used all 3 sessions with 2 audit findings open. How should it go on?" {
		t.Fatalf("the audit's findings alone: ask = %q", got)
	}
	f.ReviewFindings, f.AuditFindings = nil, nil
	if got := FaceOf(c, Facts{CaseFacts: f}).Ask; got != "The card used all 3 sessions. How should it go on?" {
		t.Fatalf("the checks' findings alone: ask = %q", got)
	}
}

// A stop at the head on a pull request that no longer merges into main
// says so in what is left, after its open findings; a head with no
// conflict says nothing of it.
func TestAStopOnAConflictedHeadSaysWhatFixesIt(t *testing.T) {
	f := CaseFacts{Kind: CaseSessionCap, Head: "h2", PR: 9, PROpen: true, Attempts: 3, SessionCap: 3, MergeConflict: "the branch conflicts with its base",
		Findings: []string{"a1"}, AuditFindings: []string{"a1"}, FindingTexts: map[string]string{"a1": "the migration runs outside its transaction"}}
	c := Case{Kind: CaseSessionCap}
	want := "1 finding is open. The pull request no longer merges into main. Only a session that merges main into its branch fixes that."
	if got := FaceOf(c, Facts{CaseFacts: f}).Story.Left; got != want {
		t.Errorf("left = %q, want %q", got, want)
	}
	f.MergeConflict = ""
	if got := FaceOf(c, Facts{CaseFacts: f}).Story.Left; got != "1 finding is open." {
		t.Errorf("with no conflict, left = %q, want the finding alone", got)
	}
}

// A budget floor after a session, on a card whose branch was read and
// holds no work, says the work is lost and the next session starts again
// from main. A branch with work on it, a branch never read (an older
// stored case) and a card that ran no session lose nothing.
func TestABudgetFloorSaysWhenTheWorkIsLost(t *testing.T) {
	const lost = "The last session's work was not pushed and is lost. The next session starts again from main."
	facts := CaseFacts{Kind: CaseBudgetFloor, BudgetEUR: 5, SpentEUR: 4.83, LeftEUR: 0.17, FloorEUR: 0.3, Attempts: 2, BranchRead: true}
	c := Case{Kind: CaseBudgetFloor}
	if left := FaceOf(c, Facts{CaseFacts: facts}).Story.Left; left != lost {
		t.Errorf("left = %q, want %q", left, lost)
	}
	for name, change := range map[string]func(f *CaseFacts){
		"a branch at c9":  func(f *CaseFacts) { f.BranchHead = "c9" },
		"a branch unread": func(f *CaseFacts) { f.BranchRead = false },
		"no session ran":  func(f *CaseFacts) { f.Attempts = 0 },
	} {
		f := facts
		change(&f)
		if left := FaceOf(c, Facts{CaseFacts: f}).Story.Left; strings.Contains(left, "is lost") {
			t.Errorf("%s: left = %q, want nothing lost", name, left)
		}
	}
}

// A cap's case says that a human lifts the cap, by a reset of its count in
// the status panel or a raise by a commit: no rung lifts it, and rung 2 only
// analyses it.
func TestTheCapRowsSayAHumanLiftsTheCap(t *testing.T) {
	for kind, name := range map[CaseKind]string{CaseDayCap: "the day cap", CaseMonthCap: "the month cap", CaseProjectCap: "the project's month cap"} {
		want := "only a human lifts " + name + ": a reset of its count in the status panel or a raise by a commit"
		if row := kindRow(kind); row.NoRung1 != want || row.Rung2Why != want || row.Rung2 != rung2Analyses {
			t.Errorf("kind %s: NoRung1 %q, Rung2 %s for %q; want both %q, rung 2 analysing", kind, row.NoRung1, row.Rung2, row.Rung2Why, want)
		}
	}
}

// Every case kind has its one row in the kind table, every row is a kind's,
// and the table says why wherever rung 2 does no more than analyse a kind,
// or nothing; its rung 0 rows are rows the ladder holds. The kinds are read
// from the source, so the next kind fails here until it has its row.
func TestTheKindTableHoldsEveryKind(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[CaseKind]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "CaseKind" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: a CaseKind constant that is no string", path)
					}
					kind, _ := strconv.Unquote(lit.Value)
					kinds[CaseKind(kind)] = true
				}
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatal("no CaseKind constant in the package")
	}
	rows := map[CaseKind]int{}
	for _, row := range caseKinds {
		rows[row.Kind]++
		if !kinds[row.Kind] {
			t.Errorf("the kind table's row %q names no CaseKind constant", row.Kind)
		}
		if row.Words == "" {
			t.Errorf("kind %s has no words", row.Kind)
		}
		switch row.Rung2 {
		case rung2Decides:
		case rung2Analyses, rung2Nothing:
			if row.Rung2Why == "" {
				t.Errorf("kind %s: rung 2 %s with no reason", row.Kind, row.Rung2)
			}
		default:
			t.Errorf("kind %s: rung 2 %q, want decides, analyses or nothing", row.Kind, row.Rung2)
		}
		for _, name := range row.Rung0 {
			if !slices.ContainsFunc(rung0, func(r resolver) bool { return r.Name == name }) {
				t.Errorf("kind %s names rung 0's row %q, which rung0 does not hold", row.Kind, name)
			}
		}
	}
	for kind := range kinds {
		if rows[kind] != 1 {
			t.Errorf("kind %s has %d rows in the kind table, want 1", kind, rows[kind])
		}
	}
}

// The door passes a case to the human when a rung 1 or rung 2 step stands
// on it, when the kind table names its kind an exception, or when rung 0's
// step leads with a mark that says no rung could judge it, as notJudged
// writes it; else it refuses, naming the case, its kind and the action that
// fits. A mark inside other words, as an agent's own reason may carry, or
// only its start, passes nothing, and neither does a mark on the problem.
func TestTheDoorPassesOnlyAClimbedExceptedOrUnjudgedCase(t *testing.T) {
	rung0Only := []CaseStep{{Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp}}
	handedUp := append(slices.Clone(rung0Only), CaseStep{Rung: 2, Actor: ChatUser, Outcome: StepHandedUp})
	checkin := append(slices.Clone(rung0Only), CaseStep{Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp})
	faced := append(slices.Clone(rung0Only), CaseStep{Rung: 3, Actor: FactoryDecider})
	const refused = "case 5 (other) has no rung 1 or rung 2 step and no exception: the case climbs first"
	other := func(problem string) Case { return Case{ID: 5, Kind: CaseOther, Problem: problem} }
	marked := func(found string) []CaseStep {
		return []CaseStep{{Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp, Found: found}}
	}
	for _, tc := range []struct {
		name  string
		c     Case
		steps []CaseStep
		want  string
	}{
		{"a rung 0 step alone", other("Which file?"), rung0Only, refused},
		{"no step and a rung 3 step", other("Which file?"), faced, refused},
		{"rung 2's hand-up", other("Which file?"), handedUp, ""},
		{"rung 1's hand-up", other("Which file?"), checkin, ""},
		{"the judgment budget with no step", Case{ID: 6, Kind: CaseJudgmentBudget, Problem: "The judgment budget is spent: raise it?"}, nil, ""},
		{"the factory's month cap with no step", Case{ID: 7, Kind: CaseFactoryMonthCap, Problem: "The month cap is reached: raise it?"}, nil, ""},
		{"the provider refused", other("Which file?"), marked("not judged: the model provider refused: the model refused the call (400)"), ""},
		{"the judgment budget spent", other("Which file?"), marked("not judged, judgment budget spent: the chat task t-chat has 0.00 of 10.00 EUR left"), ""},
		{"the judgment budget unreadable", other("Which file?"), marked("not judged, judgment budget unreadable: repository sf/r names no project"), ""},
		{"a mark inside other words", other("Which file?"), marked("The agent asks: not judged: the model provider refused"), refused},
		{"a mark's start alone", other("Which file?"), marked("not judged"), refused},
		{"a mark on the problem", other("Which file? (not judged: the model provider refused)"), rung0Only, refused},
	} {
		got := ""
		if err := doorRefusal(tc.c, climbed(tc.steps)); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: the door = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Face asks the door first, on the case as it stands: a case no rung
// worked gets no comment and no ask row, stays open and leaves the card
// as it was; once rung 2 handed it up, the same Face posts it, and the
// door puts it at the human on rung 3 with no wake.
func TestFaceOnACaseTheDoorRefusesPostsNothing(t *testing.T) {
	ctx := context.Background()
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}}
	r, store, _ := newActivityRunner(t, prTask, fj)
	r.timeline.Events = teeEvents{&fakeEventRecorder{}, store}
	due := time.Now()
	c, _, err := store.OpenCase(ctx, Case{TaskID: "t1", Repository: prTask.Repository, Kind: CaseOther, StateKey: "which", Problem: "Which file?", NextWakeAt: &due})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 0, Actor: FactoryDecider, Found: "no resolver fits", Outcome: StepHandedUp}); err != nil {
		t.Fatal(err)
	}
	const reason = "The agent asks: Which file?\nWhy: open"
	_, err = r.Face(ctx, store.tasks["t1"], c, reason, replyNext(reason), nil, nil)
	if want := fmt.Sprintf("case %d (other) has no rung 1 or rung 2 step and no exception: the case climbs first", c.ID); err == nil || err.Error() != want {
		t.Fatalf("Face = %v, want the door's refusal %q", err, want)
	}
	if got, _ := store.CaseByID(ctx, c.ID); len(fj.comments) != 0 || len(store.askedRows("t1")) != 0 || store.tasks["t1"].Status != "running" || got.Status != CaseOpen {
		t.Fatalf("comments %q, asks %+v, card %s, case %s; want nothing posted, the card running and the case open",
			fj.comments, store.askedRows("t1"), store.tasks["t1"].Status, got.Status)
	}
	if _, err := store.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Face(ctx, store.tasks["t1"], c, reason, replyNext(reason), nil, nil); err != nil {
		t.Fatalf("Face after rung 2's hand-up = %v", err)
	}
	if got, _ := store.CaseByID(ctx, c.ID); len(fj.comments) != 1 || got.Status != CaseAtHuman || got.OwnerRung != 3 || got.NextWakeAt != nil {
		t.Fatalf("comments %q, case %+v; want one face and the case at the human on rung 3 with no wake", fj.comments, got)
	}
}

// askRig is tripRig's card t1 as card store-fill of plan t-p (#30), whose
// card t-s1 (#31) closed, and the chat's human card t-h (#40), which asks
// a human about the card about names, in Needs you at once.
func askRig(t *testing.T, about string) tripRig {
	t.Helper()
	ctx := context.Background()
	g := newTripRig(t, "success")
	for _, c := range []Task{
		{ID: "t-p", Kind: "agent", Status: "running", Objective: "Land the weekly bump", Repository: "demo-app/demo-app", IssueNumber: 30, Track: policy.TrackPlan, BudgetEUR: 3},
		{ID: "t-s1", Kind: "agent", Status: "running", Objective: "Land the bump", Repository: "demo-app/demo-app", IssueNumber: 31, Track: policy.TrackDirect,
			Source: SourcePlan, Parent: "t-p", PlanKey: "land", BudgetEUR: 3},
		{ID: "t-q", Kind: "agent", Status: "running", Objective: "Another plan", Repository: "demo-app/demo-app", IssueNumber: 32, Track: policy.TrackPlan, BudgetEUR: 3},
	} {
		if err := g.s.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'closed', finished_at = now() WHERE id = 't-s1';
		UPDATE tasks SET source = 'plan', parent_task = 't-p', plan_key = 'store-fill' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	q := HumanQuestion{Ask: "Fill the file store?", Why: "The bump's build reads its new files from the store.", Info: "The store answers 404 for four files.",
		Act:     "REGISTRY_USER=… REGISTRY_PASSWORD=… scripts/upstream-files",
		Choices: []QuestionChoice{{Value: "say:Filled", Label: "Filled"}, {Value: "say:Not now", Label: "Not now"}}}
	h := Task{ID: "t-h", Source: SourceChat, Repository: "demo-app/demo-app", IssueNumber: 40, Author: "factory-agent", RiskClass: "standard",
		Objective: IssueObjective(q.Ask, "the body")}
	if _, created, err := g.s.CreateHumanCard(ctx, h, HumanAsk{Q: q, By: "sebastian", Asker: "agent", Via: ViaChat, Facts: map[string]any{FactAbout: about}}); err != nil || !created {
		t.Fatalf("CreateHumanCard: %v, %v", created, err)
	}
	return g
}

// A stop on which card t1 would ask a human, while the chat's human card
// about a card of its plan waits, asks no second time: rung 0 holds the
// case, and the card waits on that card in waiting-deps with its edge to
// it, holding no slot, no comment and no ask row of its own. The case-wake
// pass reads the ask again while it waits; once a human answered it, the
// edge goes and rung 0 resumes the card with the answer as its note, which
// the run hears. The agent's escalation and the check-in's hand-up wait
// alike.
func TestAStopWaitsOnTheChatsAskAboutItsPlan(t *testing.T) {
	for _, reason := range []string{AgentAsks + "Fill the file store?\nWhy: the build reads it\nRecommendation: run the copy",
		CheckinReason("session", "the store lacks the week's files", nil, nil)} {
		t.Run(reason[:12], func(t *testing.T) {
			ctx := context.Background()
			g := askRig(t, "t-s1")
			at := time.Date(2026, 11, 24, 15, 55, 0, 0, time.UTC)
			g.r.now = func() time.Time { return at }
			out := g.trip(t, reason)
			if out.Resolution != nil || out.Rung != 0 || out.Comment != 0 {
				t.Fatalf("the trip = %+v, want the case held at rung 0 with no face", out)
			}
			c := caseByID(t, g.s, out.CaseID)
			card, _, _ := g.s.Task(ctx, "t1")
			deps, _ := g.s.OpenDependencies(ctx, "t1")
			if c.Status != CaseOpen || c.OwnerRung != 0 || c.Facts[factWaitsOn] != "t-h" || c.NextWakeAt == nil || !c.NextWakeAt.Equal(at.Add(time.Minute)) ||
				card.Status != "waiting-deps" || card.Gate != GateDependencies || len(deps) != 1 || deps[0].TaskID != "t-h" {
				t.Fatalf("case %+v, card %s in %s, deps %+v: want the case held on t-h and the card waiting on it", c, card.Status, card.Gate, deps)
			}
			if asks := (actRig{g.s}).rows(t, EventQuestionAsked); len(asks) != 0 || len(g.forge.comments) != 0 {
				t.Fatalf("ask rows %+v, comments %q: want no second ask", asks, g.forge.comments)
			}
			m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
			wake := func(minutes int) Case {
				t.Helper()
				at = at.Add(time.Duration(minutes) * time.Minute)
				m.Now = func() time.Time { return at }
				if err := m.WakeCases(ctx); err != nil {
					t.Fatalf("WakeCases: %v", err)
				}
				return caseByID(t, g.s, out.CaseID)
			}
			if c = wake(2); c.Status != CaseOpen || c.NextWakeAt == nil || !c.NextWakeAt.Equal(at.Add(time.Minute)) {
				t.Fatalf("the case while t-h waits = %+v, want it held, its wake a minute on", c)
			}
			hc, err := g.s.NewestAskCase(ctx, "t-h")
			if err != nil || hc.Status != CaseAtHuman {
				t.Fatalf("t-h's case = %+v, %v", hc, err)
			}
			if _, err := g.r.Act(ctx, ActInput{TaskID: "t-h", CaseID: hc.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
				Res: Resolution{Action: "say:Filled", Note: "Filled"}}); err != nil {
				t.Fatalf("the answer on t-h: %v", err)
			}
			c = wake(2)
			const note = "sebastian answered #40 “Fill the file store?”, which this card waited on: Filled"
			if c.Status != CaseResolved || c.Resolution == nil || c.Resolution.Rung != 0 || c.Resolution.Action != ActResume || c.Resolution.Reason != note || c.Facts[factWaitsOn] != nil {
				t.Fatalf("the case after the answer = %+v (%+v), want rung 0's resume with the answer", c, c.Resolution)
			}
			if deps, _ := g.s.OpenDependencies(ctx, "t1"); len(deps) != 0 {
				t.Fatalf("t1's dependencies after the answer = %+v, want none", deps)
			}
			heard := slices.ContainsFunc(g.signals.got, func(s reopenCall) bool {
				return s.taskID == "t1" && s.signal == SignalCaseResolved && s.sig.Case != nil && s.sig.Case.CaseID == c.ID && s.sig.Case.Res.Note == note
			})
			if !heard {
				t.Fatalf("signals = %+v, want t1's case_resolved with the answer", g.signals.got)
			}
		})
	}
}

// A stop asks the human as before while no chat ask about its card or its
// plan waits: one about a card of another plan, or about no card, holds
// nothing.
func TestAStopAsksWhenNoChatAskIsAboutItsPlan(t *testing.T) {
	for _, about := range []string{"t-q", ""} {
		ctx := context.Background()
		g := askRig(t, about)
		out := g.trip(t, AgentAsks+"Fill the file store?\nWhy: the build reads it\nRecommendation: run the copy")
		c := caseByID(t, g.s, out.CaseID)
		card, _, _ := g.s.Task(ctx, "t1")
		if _, held := c.Facts[factWaitsOn]; held || card.Status == "waiting-deps" {
			t.Errorf("an ask about %q held the stop: case %+v, card %s", about, c, card.Status)
		}
	}
}
