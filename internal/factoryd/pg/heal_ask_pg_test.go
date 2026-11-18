package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// healAskForgejo is Forgejo as the heal pass and the re-opened ask's face
// read it: the cards' issues open with no comments, every comment posted
// on them taken and kept, and no branch of a card on the bot's fork.
type healAskForgejo struct {
	mu     sync.Mutex
	posted []string
}

func (f *healAskForgejo) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case isWriterRead(r.URL.Path):
			serveWriters(t, w, r)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/") && !strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `{"state":"open"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var c struct{ Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &c)
			f.posted = append(f.posted, c.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.posted))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") && strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// healAskRig is the heal pass's rig: a runner beside the parked-card
// store, both on one Forgejo stub.
type healAskRig struct {
	s     *PGStore
	r     *Runner
	forge *healAskForgejo
}

func newHealAskRig(t *testing.T, db string) healAskRig {
	t.Helper()
	s := cardCaseStore(t, db)
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	wf := &fakeWorkflows{}
	cfg.Workflows = wf
	forge := &healAskForgejo{}
	objects := &fakeObjectStore{}
	r := factoryd.PgRigRunner(s, objects, cfg, &Timeline{Objects: objects, Events: s, Store: s},
		&Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"})
	return healAskRig{s: s, r: r, forge: forge}
}

// pass runs one heal pass over the rig's cards.
func (g healAskRig) pass(t *testing.T) { g.passOn(t, g.s) }

// passOn runs one heal pass that reads the cards through store.
func (g healAskRig) passOn(t *testing.T, store Store) {
	t.Helper()
	tree := g.r.PgCfg().Policy().Tree
	HealPass(context.Background(), store, g.r.PgCfg().Workflows, &Intake{Store: g.s, Forgejo: g.r.PgForgejo(),
		Workflows: g.r.PgCfg().Workflows, BotUser: "factory-agent", Policy: func() policy.Tree { return tree }}, nil, noneCompleted, g.r)
}

// comments is what the face posted on the cards' issues, oldest first.
func (f *healAskForgejo) comments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posted...)
}

// park consumes the card's session-cap ask as an action-bearing answer
// row does, and ends its case resolvedAgo ago: the card keeps waiting on
// a human with no live ask. It tells the ended case.
func park(t *testing.T, s *PGStore, resolvedAgo time.Duration) Case {
	t.Helper()
	askCap(t, s, 27001)
	c := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaChat, map[string]any{
		"action": ActExtend, "agent_time_s": 1800, "comment": int64(27002)})
	if got := caseByID(t, s, c.ID); got.Status != CaseResolved || got.ResolvedAt == nil {
		t.Fatalf("the cap case = %+v; want it resolved by the answer", got)
	}
	if resolvedAgo > 0 {
		if _, err := s.DB().ExecContext(context.Background(),
			`UPDATE cases SET resolved_at = now() - $1::interval WHERE id = $2`,
			fmt.Sprintf("%d seconds", int(resolvedAgo.Seconds())), c.ID); err != nil {
			t.Fatalf("age the case: %v", err)
		}
	}
	return c
}

// A card of a running kind that waits on a human with no ask — its cap
// ask consumed by an answer, its case ended past the at_human bound —
// gets its ask back on the heal pass: the healed-run path opens the
// ask's own case again, with its face on the card's issue, so the
// console's answer paths find an ask to press. A second pass finds the
// ask live and posts no second face.
func TestHealPassReasksACardWaitingWithNoAsk(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask")
	ended := park(t, g.s, 10*time.Minute)
	if ask, err := g.s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != 0 {
		t.Fatalf("the parked card's live ask = %+v, %v; want none", ask, err)
	}

	g.pass(t)

	ask, err := g.s.NewestAskCase(ctx, caseTask)
	if err != nil || ask.ID == 0 || ask.ID == ended.ID || ask.Status != CaseAtHuman {
		t.Fatalf("the healed card's live ask = %+v, %v; want a fresh case at the human", ask, err)
	}
	reopened := ask
	posted := g.forge.comments()
	if len(posted) != 1 || !strings.Contains(posted[0], AsklessReason) {
		t.Fatalf("posted %q; want the ask's face once, asking again after the wait", posted)
	}
	asks, err := g.s.AskAnchors(ctx, caseTask)
	if err != nil || len(asks) == 0 {
		t.Fatalf("the card's ask anchors = %+v, %v; want the seeded ask's", asks, err)
	}
	if !strings.Contains(asks[len(asks)-1].Question, "session cap is reached") {
		t.Fatalf("the ask again = %q; want the words the card asked last", asks[len(asks)-1].Question)
	}

	g.pass(t)

	if ask, err := g.s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != reopened.ID {
		t.Fatalf("the ask after two passes = %+v, %v; want %d again", ask, err, reopened.ID)
	}
	if posted := g.forge.comments(); len(posted) != 1 {
		t.Fatalf("posted %q after two passes; want the one face: the ask is live again", posted)
	}
}

// The cards the pass leaves waiting as they are: one whose consumed ask
// ended inside the at_human bound, and one whose ask is live. Neither
// grows a case, and no face is posted on their issues.
func TestHealPassLeavesAskedAndFreshlyParkedCards(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_fresh")
	park(t, g.s, 0)
	if err := g.s.CreateTask(ctx, Task{ID: "t1420", Kind: "agent", Status: "waiting-human", Objective: "y",
		Repository: "sf/r", IssueNumber: 1420, Track: policy.TrackDirect, BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask t1420: %v", err)
	}
	if _, err := g.s.Record(ctx, Event{Repository: "sf/r", TaskID: "t1420", IssueNumber: 1420, Kind: EventQuestionAsked,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"question": capAsk, "ask": capAsk,
			"comment": int64(27020), "source": "cap", "reason": CapReached}}); err != nil {
		t.Fatalf("Record t1420's ask: %v", err)
	}
	live, err := g.s.OpenCasesOf(ctx, "t1420")
	if err != nil || len(live) != 1 {
		t.Fatalf("t1420's cases = %+v, %v; want its one live ask", live, err)
	}

	g.pass(t)

	cases, err := g.s.CasesOf(ctx, caseTask)
	if err != nil || len(cases) != 1 || cases[0].Status != CaseResolved {
		t.Fatalf("the freshly parked card's cases = %+v, %v; want its one ended case", cases, err)
	}
	if ask, err := g.s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != 0 {
		t.Fatalf("the freshly parked card's live ask = %+v, %v; want none: the wait is inside the bound", ask, err)
	}
	cases, err = g.s.CasesOf(ctx, "t1420")
	if err != nil || len(cases) != 1 || cases[0].ID != live[0].ID {
		t.Fatalf("the asked card's cases = %+v, %v; want its one live ask's case", cases, err)
	}
	if ask, err := g.s.NewestAskCase(ctx, "t1420"); err != nil || ask.ID != live[0].ID || ask.Status != CaseAtHuman {
		t.Fatalf("the asked card's live ask = %+v, %v; want the ask that stood", ask, err)
	}
	if posted := g.forge.comments(); len(posted) != 0 {
		t.Fatalf("posted %q; want no face: both cards wait as they are", posted)
	}
}

// askRows is how many question_asked rows the rig's card holds.
func askRows(t *testing.T, s *PGStore) int {
	t.Helper()
	rows, err := s.TaskEventsOfKind(context.Background(), caseTask, EventQuestionAsked, 1000)
	if err != nil {
		t.Fatalf("the card's ask rows: %v", err)
	}
	return len(rows)
}

// holdsAskless says case c's problem or one of its steps holds the
// askless reason.
func holdsAskless(t *testing.T, s *PGStore, c Case) bool {
	t.Helper()
	return strings.Contains(c.Problem, AsklessReason) ||
		slices.ContainsFunc(caseStepsOf(t, s, c.ID), func(st CaseStep) bool { return strings.Contains(st.Found, AsklessReason) })
}

// leavesTheWait parks the rig's card with its ended case 10 minutes old,
// past the 5-minute at_human bound, opens the live case open opens a minute
// ago and runs one heal pass: only that live case keeps the pass out, so
// the card keeps its 2 cases, no ask row is written and no face posted.
func leavesTheWait(t *testing.T, db string, open func(s *PGStore)) {
	ctx := context.Background()
	g := newHealAskRig(t, db)
	park(t, g.s, 10*time.Minute)
	open(g.s)
	live := onlyOpenCase(t, g.s)
	if _, err := g.s.DB().ExecContext(ctx, `UPDATE cases SET opened_at = now() - interval '1 minute' WHERE id = $1`, live.ID); err != nil {
		t.Fatalf("age the live case: %v", err)
	}
	before := askRows(t, g.s)

	g.pass(t)

	if cases, err := g.s.CasesOf(ctx, caseTask); err != nil || len(cases) != 2 {
		t.Fatalf("the card's cases = %+v, %v; want its ended case and its live %s", cases, err, live.Kind)
	}
	if after := askRows(t, g.s); after != before {
		t.Fatalf("question_asked rows = %d; want %d: the live %s is the card's ask", after, before, live.Kind)
	}
	if posted := g.forge.comments(); len(posted) != 0 {
		t.Fatalf("posted %q; want no face: the card waits on its %s", posted, live.Kind)
	}
}

func TestHealPassLeavesAPlanWaitingOnItsConfirmation(t *testing.T) {
	leavesTheWait(t, "heal_ask_confirm", func(s *PGStore) { askPlan(t, s, "Does the spec hold?", 1, QuestionConfirmation) })
}

func TestHealPassLeavesAPlanWaitingOnAQuestion(t *testing.T) {
	leavesTheWait(t, "heal_ask_question", func(s *PGStore) { askPlan(t, s, "Which database?", 1, "") })
}

func TestHealPassLeavesAMergeApprovalWait(t *testing.T) {
	leavesTheWait(t, "heal_ask_merge", func(s *PGStore) { openMerge(t, s, "abc1479") })
}

// A monitor flag sits beside the card's run and asks nothing: a card whose
// one live case is a flag waits with no ask, and the pass asks again.
func TestHealPassReasksWhenOnlyAMonitorFlagIsLive(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_flag")
	ended := park(t, g.s, 10*time.Minute)
	flag, _, err := g.s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: "wait_over_bound|at_human",
		Status: CaseAtHuman, Problem: "Monitor flag: wait_over_bound"})
	if err != nil {
		t.Fatalf("open the flag: %v", err)
	}

	g.pass(t)

	cases, err := g.s.CasesOf(ctx, caseTask)
	if err != nil || len(cases) != 3 {
		t.Fatalf("the card's cases = %+v, %v; want the ended case, the flag and one new ask", cases, err)
	}
	fresh := cases[2]
	if fresh.ID == ended.ID || fresh.ID == flag.ID || !holdsAskless(t, g.s, fresh) {
		t.Fatalf("the new case = %+v, steps %+v; want a step that holds the askless reason", fresh, caseStepsOf(t, g.s, fresh.ID))
	}
	if live, err := g.s.OpenCasesOf(ctx, caseTask); err != nil || len(live) != 2 || live[0].ID != flag.ID || live[1].ID != fresh.ID {
		t.Fatalf("the live cases = %+v, %v; want the flag and the new ask", live, err)
	}
}

const planConfirmText = "Does the spec of revision 1 hold?"

// planAwaitingConfirmation makes the rig's card a plan whose revision 1
// asks its confirmation under an open plan request, the confirmation's
// case withdrawn 10 minutes ago, past the at_human bound: the plan waits
// with no live case. It tells the confirmation's index.
func planAwaitingConfirmation(t *testing.T, s *PGStore) int {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `UPDATE tasks SET track = $2 WHERE id = $1`, caseTask, policy.TrackPlan); err != nil {
		t.Fatalf("make the card a plan: %v", err)
	}
	rev, err := s.AddPlanRevision(ctx, caseTask, json.RawMessage(`{"summary": "s1"}`), "s1")
	if err != nil || rev != 1 {
		t.Fatalf("AddPlanRevision = %d, %v; want revision 1", rev, err)
	}
	idx, inserted, err := s.PutPlanConfirmation(ctx, caseTask, rev, factoryd.PlanQuestion{Text: planConfirmText})
	if err != nil || !inserted {
		t.Fatalf("PutPlanConfirmation = %d, %t, %v", idx, inserted, err)
	}
	spec, err := json.Marshal(factoryd.PlanSpec{Revision: rev, SHA256: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenApproval(ctx, ApprovalRequest{Kind: factoryd.ApprovalPlan, TaskID: caseTask, Project: "sf/r", Spec: spec}); err != nil {
		t.Fatalf("OpenApproval: %v", err)
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": planConfirmText, "revision": rev, "index": idx, "kind": QuestionConfirmation})
	c := onlyOpenCase(t, s)
	if err := s.WithdrawCase(ctx, c.ID, "withdrawn while the plan waited"); err != nil {
		t.Fatalf("withdraw the confirmation's case: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE cases SET resolved_at = now() - interval '10 minutes' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("age the case: %v", err)
	}
	return idx
}

// A plan whose newest revision waits on its confirmation under its
// request, its case gone, gets that case back from its own ask row, with
// the same key: the plan's answer path resolves it, and no askless ask is
// put to the human.
func TestHealPassRestoresAPlansConfirmation(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_restore")
	idx := planAwaitingConfirmation(t, g.s)

	g.pass(t)

	restored := onlyOpenCase(t, g.s)
	if restored.Kind != factoryd.CasePlanConfirmation || !strings.HasSuffix(restored.StateKey, "@r1") || restored.Status != CaseAtHuman {
		t.Fatalf("the live case = %+v; want revision 1's confirmation at the human", restored)
	}
	cases, err := g.s.CasesOf(ctx, caseTask)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if holdsAskless(t, g.s, c) {
			t.Fatalf("case %+v holds the askless reason; want the plan's own ask alone", c)
		}
	}
	if posted := g.forge.comments(); len(posted) != 0 {
		t.Fatalf("posted %q; want no face: the plan's ask is its own case", posted)
	}

	if err := factoryd.AnswerPlan(ctx, g.s, g.r.PgCfg().Workflows, caseTask, 1, idx, factoryd.DecisionConfirm, "", "sebastian"); err != nil {
		t.Fatalf("AnswerPlan: %v", err)
	}
	recordCaseEvent(t, g.s, factoryd.EventPlanQuestionAnswered, "sebastian", ViaConsole, map[string]any{
		"revision": 1, "index": idx, "decision": factoryd.DecisionConfirm})
	if got := caseByID(t, g.s, restored.ID); got.Status != CaseResolved {
		t.Fatalf("the restored case after the answer = %+v; want it resolved", got)
	}
}

// approvalsDown is the rig's store with its approval requests unreadable.
type approvalsDown struct{ *PGStore }

func (approvalsDown) ApprovalRequests(context.Context, string) ([]ApprovalRequest, error) {
	return nil, errors.New("the approval requests cannot be read")
}

// A plan whose request cannot be read may well wait on its questions: the
// pass asks nothing of it, neither its questions nor the askless ask.
func TestHealPassLeavesAPlanWhoseRequestCannotBeRead(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_request_down")
	planAwaitingConfirmation(t, g.s)

	g.passOn(t, approvalsDown{g.s})

	if cases, err := g.s.CasesOf(ctx, caseTask); err != nil || len(cases) != 1 || cases[0].Status != CaseWithdrawn {
		t.Fatalf("the plan's cases = %+v, %v; want its withdrawn confirmation alone", cases, err)
	}
	if posted := g.forge.comments(); len(posted) != 0 {
		t.Fatalf("posted %q; want no face", posted)
	}
}

// A plan whose newest revision holds every answer has no question to ask
// again: the pass leaves the plan's cases be and asks the stop's
// askless ask.
func TestHealPassLeavesAnAnsweredRevisionToTheStop(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_answered")
	idx := planAwaitingConfirmation(t, g.s)
	if err := g.s.AnswerPlanQuestion(ctx, caseTask, 1, idx, factoryd.DecisionConfirm, "", "sebastian"); err != nil {
		t.Fatalf("AnswerPlanQuestion: %v", err)
	}

	g.pass(t)

	live := onlyOpenCase(t, g.s)
	if live.Kind == factoryd.CasePlanConfirmation || live.Kind == CasePlanQuestion {
		t.Fatalf("the live case = %+v; want no plan case", live)
	}
	if live.Problem != AsklessReason || !holdsAskless(t, g.s, live) {
		t.Fatalf("the new case = %+v, steps %+v; want the askless reason as its problem and its step", live, caseStepsOf(t, g.s, live.ID))
	}
	if cases, err := g.s.CasesOf(ctx, caseTask); err != nil || len(cases) != 2 {
		t.Fatalf("the card's cases = %+v, %v; want the withdrawn confirmation and one new case", cases, err)
	}
}

// The wait began at the newest end of an ask, not of a flag: an ask that
// ended 2 minutes ago keeps the card inside the 5-minute bound, though a
// flag of a higher id ended 10 minutes ago.
func TestAsklessSinceSkipsMonitorFlags(t *testing.T) {
	ctx := context.Background()
	g := newHealAskRig(t, "heal_ask_since")
	park(t, g.s, 2*time.Minute)
	flag, _, err := g.s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: "wait_over_bound|at_human",
		Status: CaseAtHuman, Problem: "Monitor flag: wait_over_bound"})
	if err != nil {
		t.Fatalf("open the flag: %v", err)
	}
	if err := g.s.WithdrawCase(ctx, flag.ID, "the flag settled"); err != nil {
		t.Fatalf("withdraw the flag: %v", err)
	}
	if _, err := g.s.DB().ExecContext(ctx, `UPDATE cases SET resolved_at = now() - interval '10 minutes' WHERE id = $1`, flag.ID); err != nil {
		t.Fatalf("age the flag: %v", err)
	}

	g.pass(t)

	if cases, err := g.s.CasesOf(ctx, caseTask); err != nil || len(cases) != 2 {
		t.Fatalf("the card's cases = %+v, %v; want the ended ask and the flag alone: the wait is 2 minutes old", cases, err)
	}
	if posted := g.forge.comments(); len(posted) != 0 {
		t.Fatalf("posted %q; want no face", posted)
	}
}
