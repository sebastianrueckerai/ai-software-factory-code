package factoryd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
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
// no retry, and the human is asked.
func TestOpenTripRetriesAFailedDependencyOnce(t *testing.T) {
	ctx := context.Background()
	r, store, fj := outsidePlanRig(t)
	rungsOff(r)
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
	if second.Resolution != nil || second.Comment == 0 || second.Rung != 3 {
		t.Fatalf("second outcome = %+v, want the human asked", second)
	}
	c, _ = store.CaseByID(ctx, second.CaseID)
	steps, _ := store.CaseSteps(ctx, c.ID)
	if c.StateKey != again || c.Status != CaseAtHuman || len(steps) != 2 || steps[0].Found != "no resolver fits" || steps[0].Outcome != StepHandedUp ||
		steps[1].Rung != 3 || steps[1].CommentID != second.Comment {
		t.Fatalf("case %+v, steps %+v; want the replacement's failure at the human, rung 0 finding no resolver, then the face at comment %d", c, steps, second.Comment)
	}
	if _, ok := store.tasks[RetryID(again)]; ok {
		t.Fatal("the replacement was retried in turn")
	}
}

// Rung 0's rows on a case's kind and facts, the first that fires winning:
// a green approved head goes to its approval, a red one's checks run once,
// a cap of the day waits, and a failed dependency is retried outside a plan
// and sent to its planner inside one, unless it is a retry itself.
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
		{"green, not approved", CaseGateStop, head("green", false), nil},
		{"red", CaseBudgetFloor, head("red", false), &Resolution{Action: ActRerunChecks}},
		{"red, run again", CaseCheckinShare, func() CaseFacts { f := head("red", false); f.RerunOnHead = true; return f }(), nil},
		{"a checks wait's stop with every check green", CaseGateStop, checksWait("green", false), &Resolution{Action: ActWaitChecks}},
		{"a checks wait's stop, waited on this head already", CaseGateStop, checksWait("green", true), nil},
		{"a checks wait's stop with red checks", CaseGateStop, checksWait("red", false), &Resolution{Action: ActRerunChecks}},
		{"a checks wait's stop with checks pending", CaseGateStop, checksWait("pending", false), nil},
		{"green at a stop of no head", CaseAgentEscalate, head("green", true), nil},
		{"the day cap", CaseDayCap, head("green", true), nil},
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
	if face := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: card}); face.Ask != "The card used all 3 sessions. Allow more, or end the card?" {
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
// of the failed card's issue fails after the replacement is filed, and the
// human is asked.
func TestOpenTripHandsUpARowThatFails(t *testing.T) {
	ctx := context.Background()
	r, store, fj := outsidePlanRig(t)
	rungsOff(r)
	fj.failCall = "PATCH issues/5"
	out, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: DepFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := store.CaseByID(ctx, out.CaseID)
	steps, _ := store.CaseSteps(ctx, c.ID)
	if out.Resolution != nil || out.Comment == 0 || c.Status != CaseAtHuman || len(steps) != 2 ||
		!strings.HasPrefix(steps[0].Found, "retry_dependency failed: ") || steps[0].Outcome != StepHandedUp || steps[1].Rung != 3 || steps[1].CommentID != out.Comment {
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
// read, rung 0 takes nothing, its step names the read that failed, and
// the human is asked.
func TestRungZeroTakesNothingOnFactsReadPartly(t *testing.T) {
	trip := func(t *testing.T, unread bool) (factsRig, TripOutcome) {
		g := newFactsRig(t)
		rungsOff(g.r)
		forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/demo-app/demo-app/commits/h2/status":
				fmt.Fprint(w, `{"state":"success","statuses":[{"status":"success","context":"ci / test"}]}`)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/demo-app/demo-app/issues/12/comments":
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"id": 501}`)
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
		if out.Resolution != nil || out.Comment != 501 || out.Rung != 3 {
			t.Fatalf("outcome = %+v, want the human asked on comment 501", out)
		}
		c, _ := g.store.CaseByID(context.Background(), out.CaseID)
		steps, _ := g.store.CaseSteps(context.Background(), out.CaseID)
		if c.Status != CaseAtHuman || len(steps) != 1 || steps[0].Found != "facts read partly: timeline, so rung 0 took nothing" || steps[0].Outcome != StepHandedUp {
			t.Fatalf("case %+v, steps %+v; want it at the human, rung 0 handing up on the unread timeline", c, steps)
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
	if got := FaceOf(c, Facts{CaseFacts: f}).Ask; got != "The card used all 3 sessions with 2 findings open. Allow more, or end the card?" {
		t.Fatalf("ask = %q", got)
	}
	audit := f
	audit.ReviewFindings, audit.AuditFindings = nil, []string{"r1", "a1"}
	if got := FaceOf(c, Facts{CaseFacts: audit}).Ask; got != "The card used all 3 sessions with 2 audit findings open. Allow more, or end the card?" {
		t.Fatalf("the audit's findings alone: ask = %q", got)
	}
	f.ReviewFindings, f.AuditFindings = nil, nil
	if got := FaceOf(c, Facts{CaseFacts: f}).Ask; got != "The card used all 3 sessions. Allow more, or end the card?" {
		t.Fatalf("the checks' findings alone: ask = %q", got)
	}
}
