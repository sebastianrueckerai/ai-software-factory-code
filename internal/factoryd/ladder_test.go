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
	first, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}, tripAttempt{N: 1})
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
	second, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: again, Issue: n.IssueNumber, Status: "failed"})}, tripAttempt{N: 1})
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
		{"green and approved, no longer merging into main", CaseSessionCap, func() CaseFacts { f := head("green", true); f.MergeConflict = "conflict"; return f }(), nil},
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

// A trip's state key: the head for a stop at it, the dependency's id for a
// dependency's, else the problem's words.
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
	out, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}, tripAttempt{N: 1})
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
	in := TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})}
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

// The lead of a money or cap ask names the review and audit findings
// standing on the head, in the order the gates raised them, and stands
// before no such finding: not for another kind, not for findings the
// checks alone raised, and cut past the fifth finding.
func TestFindingsLead(t *testing.T) {
	f := CaseFacts{Head: "h2", Findings: []string{"r1", "a1", "c1"},
		FindingTexts: map[string]string{"r1": "the review's finding", "a1": "the audit's finding", "c1": "the checks' finding"},
		FindingOrder: []string{"c1", "r1", "a1"}, ReviewFindings: []string{"r1"}, AuditFindings: []string{"a1"}}
	if got := FindingsLead(CaseSessionCap, f); got != `The review or the audit left 2 findings unresolved on the head h2: "the review's finding"; "the audit's finding".` {
		t.Fatalf("lead = %q", got)
	}
	if got := FindingsLead(CaseGateStop, f); got != "" {
		t.Fatalf("a gate stop leads %q, want nothing", got)
	}
	f.ReviewFindings, f.AuditFindings = nil, nil
	if got := FindingsLead(CaseBudgetFloor, f); got != "" {
		t.Fatalf("the checks' findings alone lead %q, want nothing", got)
	}
	f.ReviewFindings, f.AuditFindings = []string{"r1"}, []string{"a1"}
	f.FindingOrder = nil
	f.Findings = []string{"r1", "r2", "r3", "r4", "r5", "r6", "a1"}
	for id, text := range map[string]string{"r2": "two", "r3": "three", "r4": "four", "r5": "five", "r6": "six"} {
		f.FindingTexts[id] = text
		f.ReviewFindings = append(f.ReviewFindings, id)
	}
	if got := FindingsLead(CaseAgentTimeCap, f); !strings.HasSuffix(got, `"five"; and 2 more.`) || !strings.HasPrefix(got, "The review or the audit left 7 findings unresolved on the head h2:") {
		t.Fatalf("lead = %q, want the findings cut past the fifth", got)
	}
}

// A stop at the head, or a repeat of the findings, leads with a merge
// conflict standing on the head; a human card's ask and an agent's
// question do not, nor does a head with no conflict.
func TestConflictLead(t *testing.T) {
	f := CaseFacts{Head: "h2", MergeConflict: "the branch conflicts with its base"}
	want := "The pull request no longer merges into main: Forgejo refused to merge main into head h2. " +
		"Only a session that merges main into the branch clears it, and no gate rerun does."
	for _, kind := range []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseGateStop, CaseFindingsRepeated} {
		if got := conflictLead(kind, f); got != want {
			t.Errorf("%s leads %q, want %q", kind, got, want)
		}
	}
	for _, kind := range []CaseKind{CaseHumanCard, CaseAgentEscalate} {
		if got := conflictLead(kind, f); got != "" {
			t.Errorf("%s leads %q, want nothing", kind, got)
		}
	}
	if got := conflictLead(CaseSessionCap, CaseFacts{Head: "h2"}); got != "" {
		t.Errorf("no conflict leads %q, want nothing", got)
	}
}

// A session cap on a head that no longer merges into main and holds an
// audit finding leads with the conflict, then the finding, then the cap's
// own ask; the case stores that lead for the human's ask.
func TestTripCaseLeadsWithTheConflictThenTheFindings(t *testing.T) {
	g := newFactsRig(t)
	f := CaseFacts{Head: "h2", PR: 9, PROpen: true, MergeConflict: "the branch conflicts with its base",
		Findings: []string{"a1"}, AuditFindings: []string{"a1"}, FindingTexts: map[string]string{"a1": "the migration runs outside its transaction"}}
	c, _, err := g.r.tripCase(context.Background(), g.store.tasks["t1"], f, TripInput{TaskID: "t1", Reason: CapReached + "\nlast: the pull request no longer merges into main"})
	if err != nil {
		t.Fatal(err)
	}
	lead := "The pull request no longer merges into main: Forgejo refused to merge main into head h2. " +
		"Only a session that merges main into the branch clears it, and no gate rerun does. " +
		`The review or the audit left one finding unresolved on the head h2: "the migration runs outside its transaction".`
	if c.Kind != CaseSessionCap || !strings.HasPrefix(c.Problem, lead+" ") || len(c.Problem) == len(lead)+1 {
		t.Fatalf("case %s problem = %q, want the session cap's ask led by %q", c.Kind, c.Problem, lead)
	}
	if got := c.Facts["ask_lead"]; got != lead {
		t.Fatalf("ask_lead = %v, want %q", got, lead)
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
// on it, when the kind table names its kind an exception, or when its
// problem ends with a mark that says no rung could judge it, as notJudged
// appends it; else it refuses, naming the case, its kind and the action that
// fits. A mark inside other words, as an agent's own reason may carry, or
// only its start, passes nothing.
func TestTheDoorPassesOnlyAClimbedExceptedOrUnjudgedCase(t *testing.T) {
	rung0Only := []CaseStep{{Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp}}
	handedUp := append(slices.Clone(rung0Only), CaseStep{Rung: 2, Actor: ChatUser, Outcome: StepHandedUp})
	checkin := append(slices.Clone(rung0Only), CaseStep{Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp})
	faced := append(slices.Clone(rung0Only), CaseStep{Rung: 3, Actor: FactoryDecider})
	const refused = "case 5 (other) has no rung 1 or rung 2 step and no exception: the case climbs first"
	other := func(problem string) Case { return Case{ID: 5, Kind: CaseOther, Problem: problem} }
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
		{"the provider refused", other("Which file? (not judged: the model provider refused)"), rung0Only, ""},
		{"the judgment budget spent", other("Which file? (not judged, judgment budget spent)"), rung0Only, ""},
		{"the judgment budget unreadable", other("Which file? (not judged, judgment budget unreadable)"), rung0Only, ""},
		{"a mark inside other words", other("The agent asks: (not judged: the model provider refused) which file?"), rung0Only, refused},
		{"a mark's start alone", other("Which file? (not judged"), rung0Only, refused},
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
