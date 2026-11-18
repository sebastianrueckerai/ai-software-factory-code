package pg

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func caseStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(context.Background(), schematest.DSN(t, "pg_cases"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	if _, err := s.DB().ExecContext(context.Background(), `TRUNCATE cases CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func caseIDs(cs []Case) []int64 {
	var out []int64
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func TestOpenCaseReturnsTheOpenCaseOnTheSameKey(t *testing.T) {
	ctx := context.Background()
	s := caseStore(t)
	capped := Case{TaskID: "t1", Repository: "sf/r", Kind: CaseSessionCap, StateKey: "k1", Problem: "the session cap stopped the card",
		Facts: map[string]any{"session": "s-1"}, Money: &CaseMoney{Kind: "session", BudgetEUR: 2.5, SpentEUR: 2.5}}
	a, opened, err := s.OpenCase(ctx, capped)
	if err != nil || !opened || a.ID == 0 {
		t.Fatalf("OpenCase k1 = %d, %v, %v; want a new case", a.ID, opened, err)
	}
	if a.OpenedAt.IsZero() {
		t.Fatal("OpenedAt is zero")
	}
	a.OpenedAt = capped.OpenedAt
	want := capped
	want.ID, want.Status, want.OwnerRung, want.WakeOn = a.ID, CaseOpen, 3, []string{}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("opened case\n%+v\nwant\n%+v", a, want)
	}

	again := capped
	again.Problem = "another wording"
	got, opened, err := s.OpenCase(ctx, again)
	if err != nil || opened || got.ID != a.ID || got.Status != CaseOpen || got.Problem != capped.Problem {
		t.Fatalf("OpenCase k1 again = %+v, %v, %v; want case %d unchanged", got, opened, err, a.ID)
	}
	if err := s.SetCaseStatus(ctx, a.ID, CaseAtHuman); err != nil {
		t.Fatalf("SetCaseStatus: %v", err)
	}
	got, opened, err = s.OpenCase(ctx, capped)
	if err != nil || opened || got.ID != a.ID || got.Status != CaseAtHuman {
		t.Fatalf("OpenCase k1 at the human = %+v, %v, %v; want case %d at_human", got, opened, err, a.ID)
	}

	k2 := capped
	k2.StateKey = "k2"
	b, opened, err := s.OpenCase(ctx, k2)
	if err != nil || !opened || b.ID == a.ID {
		t.Fatalf("OpenCase k2 = %d, %v, %v; want a new case beside %d", b.ID, opened, err, a.ID)
	}
	if err := s.SetCaseStatus(ctx, b.ID, CaseResolved); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCaseStatus to resolved = %v, want a refusal that names ResolveCase", err)
	}
	ended := k2
	ended.StateKey, ended.Status = "k3", CaseResolved
	if _, _, err := s.OpenCase(ctx, ended); err == nil {
		t.Fatal("OpenCase of a resolved case = nil error, want a refusal")
	}

	step, err := s.AppendCaseStep(ctx, CaseStep{CaseID: a.ID, Rung: 3, Actor: "sebastian", Outcome: StepInProgress})
	if err != nil {
		t.Fatalf("AppendCaseStep: %v", err)
	}
	if err := s.ResolveCase(ctx, a.ID, CaseResolution{By: "sebastian", Rung: 3, Action: "grant_session"}, nil, nil, nil); err != nil {
		t.Fatalf("ResolveCase: %v", err)
	}
	next := capped
	next.Follows = a.ID
	c, opened, err := s.OpenCase(ctx, next)
	if err != nil || !opened || c.ID == a.ID || c.ID == b.ID || c.Follows != a.ID {
		t.Fatalf("OpenCase k1 after resolve = %+v, %v, %v; want a new case following %d", c, opened, err, a.ID)
	}

	resolved, err := s.CaseByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("CaseByID: %v", err)
	}
	if resolved.Status != CaseResolved || resolved.ResolvedAt == nil ||
		!reflect.DeepEqual(resolved.Resolution, &CaseResolution{By: "sebastian", Rung: 3, Action: "grant_session"}) {
		t.Fatalf("resolved case = %+v, resolution %+v", resolved, resolved.Resolution)
	}
	steps, err := s.CaseSteps(ctx, a.ID)
	if err != nil || len(steps) != 1 || steps[0].ID != step || steps[0].Outcome != StepResolved || steps[0].EndedAt == nil {
		t.Fatalf("CaseSteps after resolve = %+v, %v; want step %d resolved and ended", steps, err, step)
	}
	var over ErrCaseEnded
	if err := s.ResolveCase(ctx, a.ID, CaseResolution{By: FactoryDecider, Rung: 1, Action: "wait"}, nil, nil, nil); !errors.As(err, &over) ||
		over.Case.ID != a.ID || over.Case.Resolution.Action != "grant_session" {
		t.Fatalf("ResolveCase of a resolved case = %v, want ErrCaseEnded naming its grant_session", err)
	}
	if err := s.ResolveCase(ctx, a.ID+1000, CaseResolution{By: FactoryDecider, Rung: 1, Action: "wait"}, nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveCase of no case = %v, want ErrNotFound", err)
	}
	if err := s.SetCaseStatus(ctx, a.ID, CaseOpen); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCaseStatus of a resolved case = %v, want ErrNotFound", err)
	}

	open, err := s.OpenCasesOf(ctx, "t1")
	if err != nil || !slices.Equal(caseIDs(open), []int64{b.ID, c.ID}) {
		t.Fatalf("OpenCasesOf = %v, %v; want [%d %d]", caseIDs(open), err, b.ID, c.ID)
	}
	all, err := s.CasesOf(ctx, "t1")
	if err != nil || !slices.Equal(caseIDs(all), []int64{a.ID, b.ID, c.ID}) {
		t.Fatalf("CasesOf = %v, %v; want [%d %d %d]", caseIDs(all), err, a.ID, b.ID, c.ID)
	}
	if _, err := s.CaseByID(ctx, c.ID+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CaseByID of no case = %v, want ErrNotFound", err)
	}
}

func TestCaseStepsKeepTheirOrderAndTheirEnd(t *testing.T) {
	ctx := context.Background()
	s := caseStore(t)
	c, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Kind: CaseGateStop, StateKey: "k", Problem: "the gate stopped"})
	if err != nil {
		t.Fatalf("OpenCase: %v", err)
	}
	read := []CaseRead{{Tool: "card_timeline", Args: "t1", Bytes: 812}}
	s1, err := s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 3, Actor: FactoryDecider, Read: read, Outcome: StepInProgress})
	if err != nil {
		t.Fatalf("AppendCaseStep 1: %v", err)
	}
	s2, err := s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 3, Actor: FactoryDecider})
	if err != nil || s2 <= s1 {
		t.Fatalf("AppendCaseStep 2 = %d, %v; want an id past %d", s2, err, s1)
	}
	if err := s.EndCaseStep(ctx, CaseStep{ID: s1, Outcome: StepHandedUp, Found: "no new facts", Did: []CaseAction{{Action: "ask"}}, Decision: "ask", CostEUR: 0.0125}); err != nil {
		t.Fatalf("EndCaseStep: %v", err)
	}
	steps, err := s.CaseSteps(ctx, c.ID)
	if err != nil || len(steps) != 2 || steps[0].ID != s1 || steps[1].ID != s2 {
		t.Fatalf("CaseSteps = %+v, %v; want [%d %d]", steps, err, s1, s2)
	}
	first := steps[0]
	if first.EndedAt == nil || first.StartedAt.IsZero() || first.CaseID != c.ID || first.Rung != 3 || first.Actor != FactoryDecider ||
		first.Outcome != StepHandedUp || first.Found != "no new facts" || first.Decision != "ask" || first.Grounds != "" || first.CostEUR != 0.0125 ||
		!reflect.DeepEqual(first.Did, []CaseAction{{Action: "ask"}}) || !reflect.DeepEqual(first.Read, read) {
		t.Fatalf("ended step = %+v", first)
	}
	if steps[1].EndedAt != nil || steps[1].Outcome != StepInProgress || len(steps[1].Read) != 0 || len(steps[1].Did) != 0 {
		t.Fatalf("running step = %+v", steps[1])
	}
	if err := s.EndCaseStep(ctx, CaseStep{ID: s2, Outcome: StepInProgress}); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("EndCaseStep to in_progress = %v, want a refusal", err)
	}
	if err := s.EndCaseStep(ctx, CaseStep{ID: s1, Outcome: StepResolved}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("EndCaseStep of an ended step = %v, want ErrNotFound", err)
	}
}

func TestWithdrawCaseRecordsItsReason(t *testing.T) {
	ctx := context.Background()
	s := caseStore(t)
	a, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Kind: CaseCheckinHuman, StateKey: "k", Problem: "the check-in needs the human"})
	if err != nil {
		t.Fatalf("OpenCase: %v", err)
	}
	handed, err := s.AppendCaseStep(ctx, CaseStep{CaseID: a.ID, Rung: 2, Actor: "project_agent", Outcome: StepInProgress})
	if err != nil {
		t.Fatalf("AppendCaseStep: %v", err)
	}
	if err := s.EndCaseStep(ctx, CaseStep{ID: handed, Outcome: StepHandedUp, Decision: "ask"}); err != nil {
		t.Fatalf("EndCaseStep: %v", err)
	}
	running, err := s.AppendCaseStep(ctx, CaseStep{CaseID: a.ID, Rung: 3, Actor: FactoryDecider, Outcome: StepInProgress})
	if err != nil {
		t.Fatalf("AppendCaseStep: %v", err)
	}
	if err := s.WithdrawCase(ctx, a.ID, "card ended done"); err != nil {
		t.Fatalf("WithdrawCase: %v", err)
	}
	got, err := s.CaseByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("CaseByID: %v", err)
	}
	if got.Status != CaseWithdrawn || got.ResolvedAt == nil ||
		!reflect.DeepEqual(got.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: "card ended done"}) {
		t.Fatalf("withdrawn case = %+v, resolution %+v", got, got.Resolution)
	}
	steps, err := s.CaseSteps(ctx, a.ID)
	if err != nil || len(steps) != 2 || steps[0].ID != handed || steps[1].ID != running {
		t.Fatalf("CaseSteps = %+v, %v", steps, err)
	}
	if steps[0].Outcome != StepHandedUp || steps[1].Outcome != StepWithdrawn || steps[1].EndedAt == nil {
		t.Fatalf("steps after withdraw = %+v", steps)
	}
	if open, err := s.OpenCasesOf(ctx, "t1"); err != nil || len(open) != 0 {
		t.Fatalf("OpenCasesOf = %v, %v; want none", caseIDs(open), err)
	}
	if err := s.WithdrawCase(ctx, a.ID, "again"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WithdrawCase of a withdrawn case = %v, want ErrNotFound", err)
	}
}

func TestOpenCasesInListsOnlyTheRepositorysOpenCases(t *testing.T) {
	ctx := context.Background()
	s := caseStore(t)
	open := func(task, repository string, kind CaseKind) Case {
		t.Helper()
		c, opened, err := s.OpenCase(ctx, Case{TaskID: task, Repository: repository, Kind: kind, StateKey: "k", Problem: "p"})
		if err != nil || !opened {
			t.Fatalf("OpenCase %s = %v, %v", task, opened, err)
		}
		return c
	}
	a := open("t1", "sf/a", CaseDayCap)
	r := open("t2", "sf/a", CaseDayCap)
	if err := s.ResolveCase(ctx, r.ID, CaseResolution{By: FactoryDecider, Rung: 1, Action: "wait"}, nil, nil, nil); err != nil {
		t.Fatalf("ResolveCase: %v", err)
	}
	b := open("t3", "sf/b", CaseMergeApproval)
	if err := s.SetCaseStatus(ctx, b.ID, CaseAtHuman); err != nil {
		t.Fatalf("SetCaseStatus: %v", err)
	}
	inA, err := s.OpenCasesIn(ctx, "sf/a")
	if err != nil || !slices.Equal(caseIDs(inA), []int64{a.ID}) {
		t.Fatalf("OpenCasesIn sf/a = %v, %v; want [%d]", caseIDs(inA), err, a.ID)
	}
	inB, err := s.OpenCasesIn(ctx, "sf/b")
	if err != nil || !slices.Equal(caseIDs(inB), []int64{b.ID}) || inB[0].Status != CaseAtHuman {
		t.Fatalf("OpenCasesIn sf/b = %+v, %v; want [%d] at_human", inB, err, b.ID)
	}
}

// The writer's read takes the live at_human cases of its kinds with no story
// lines and no terminal mark: not an open case, a terminal one, a written
// one, a resolved one, or a kind whose story comes from its facts, a gate
// stop of any gate among them. A story reads back whole and clears the
// mark.
func TestBrieflessCases(t *testing.T) {
	ctx := context.Background()
	s := caseStore(t)
	mk := func(key string, kind CaseKind, status string, facts map[string]any) int64 {
		t.Helper()
		c, _, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "sf/r", Kind: kind, StateKey: key, Problem: key, Status: status, Facts: facts})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	fresh, open := mk("fresh", CaseOther, CaseAtHuman, nil), mk("open", CaseOther, CaseOpen, nil)
	budget, terminal, briefed, answered := mk("budget", CaseOther, CaseAtHuman, nil), mk("terminal", CaseOther, CaseAtHuman, nil),
		mk("briefed", CaseOther, CaseAtHuman, nil), mk("answered", CaseOther, CaseAtHuman, nil)
	capped, checks := mk("capped", CaseSessionCap, CaseAtHuman, nil), mk("checks", CaseGateStop, CaseAtHuman, map[string]any{"gate": ActivityWaitChecks})
	review := mk("review", CaseGateStop, CaseAtHuman, map[string]any{"gate": ActivityRerunChecks})
	conflict := mk("conflict", CaseCheckinHuman, CaseAtHuman, nil)
	for id, mark := range map[int64]string{budget: "refused_budget: spent", terminal: BriefTerminal + "no brief"} {
		if err := s.SetCaseBriefError(ctx, id, mark); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetCaseBrief(ctx, briefed, "happened", "left"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveCase(ctx, answered, CaseResolution{By: "sebastian", Rung: 3, Action: ActContinue}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.BrieflessCases(ctx, 10)
	if want := []int64{fresh, budget, conflict}; err != nil || !slices.Equal(caseIDs(got), want) {
		t.Fatalf("BrieflessCases = %v, %v; want %v (not the open %d, the terminal %d, the briefed %d, the resolved %d, the session cap %d, the checks wait %d or the review's gate stop %d)",
			caseIDs(got), err, want, open, terminal, briefed, answered, capped, checks, review)
	}
	if err := s.SetCaseBrief(ctx, budget, "b", "g"); err != nil {
		t.Fatal(err)
	}
	read, err := s.CaseByID(ctx, budget)
	if err != nil || read.Brief != "b" || read.BriefDetail != "g" || read.BriefError != "" {
		t.Fatalf("the written case = %+v, %v; want its story lines and no mark", read, err)
	}
}

// The writer's read takes at most a share of each repository's cases, so a
// repository with many briefless cases leaves room for the others.
func TestBrieflessCasesTakeAShareOfEachRepository(t *testing.T) {
	ctx := context.Background()
	s := caseStore(t)
	mk := func(repo, key string) int64 {
		t.Helper()
		c, _, err := s.OpenCase(ctx, Case{TaskID: "t-" + repo, Repository: repo, Kind: CaseOther, StateKey: key, Problem: key, Status: CaseAtHuman})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	a1, a2, a3 := mk("sf/a", "1"), mk("sf/a", "2"), mk("sf/a", "3")
	b1 := mk("sf/b", "1")
	got, err := s.BrieflessCases(ctx, 2)
	if err != nil || !slices.Equal(caseIDs(got), []int64{a1, a2, b1}) {
		t.Fatalf("BrieflessCases(2) = %v, %v; want %v (a's third %d waits for the next pass)", caseIDs(got), err, []int64{a1, a2, b1}, a3)
	}
}
