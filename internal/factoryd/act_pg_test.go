package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// actRig is a runner on Postgres with card t1, a large agent card of
// demo-app/demo-app on the direct track, waiting at a human on issue 12
// with a budget of 3 EUR under the policy's ceiling of 5 EUR, the default
// pool (30 sessions, 30 hours, 60 EUR) and card draw. Its live session cap
// case was asked with no comment of its own. Forgejo takes comments on the
// project's issues (ids 501 on) and closes them, and the workflows record
// every signal.
type actRig struct {
	r       *Runner
	s       *PGStore
	c       Case
	signals *caseSignals
	forge   *actForge
}

// caseSignals records the signals that reached the workflows.
type caseSignals struct {
	Workflows
	mu  sync.Mutex
	got []reopenCall
}

func (w *caseSignals) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, reopenCall{taskID, name, sig})
	return nil
}

// actForge is a Forgejo for the issues of demo-app/demo-app: it keeps the
// comments posted and whether issue 12 was closed.
type actForge struct {
	mu         sync.Mutex
	comments   []string
	closed     bool
	closes     int  // the closes of issue 12 that went through
	closeFails bool // the close of issue 12 answers 500
}

func (f *actForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const issue = "/api/v1/repos/demo-app/demo-app/issues/12"
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/repos/demo-app/demo-app/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			var c struct{ Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &c)
			f.comments = append(f.comments, c.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.comments))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/demo-app/demo-app/pulls":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == issue:
			fmt.Fprint(w, `{"state":"open"}`)
		case r.Method == http.MethodGet && r.URL.Path == issue+"/dependencies":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch && r.URL.Path == issue && f.closeFails:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPatch && r.URL.Path == issue:
			f.closed = true
			f.closes++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newActRig(t *testing.T) actRig {
	t.Helper()
	ctx := context.Background()
	s := cardCaseStore(t, "act")
	if err := s.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 12, Track: policy.TrackDirect, Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	c, opened, err := s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap",
		Status: CaseAtHuman, Problem: "Card #12 used its sessions"})
	if err != nil || !opened {
		t.Fatalf("OpenCase = %+v, %v, %v", c, opened, err)
	}
	v := policy.Version{ID: 7, Tree: projectTree(t)}
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	signals, forge := &caseSignals{}, &actForge{}
	cfg.Workflows = signals
	objects := &fakeObjectStore{}
	r := &Runner{store: s, objects: objects, cfg: cfg, timeline: &Timeline{Objects: objects, Events: s, Store: s},
		forgejo: &Forgejo{BaseURL: forge.serve(t).URL, BotUser: "factory-agent", BotPassword: "pw"}}
	return actRig{r: r, s: s, c: c, signals: signals, forge: forge}
}

// rows are card t1's events of kind, oldest first.
func (g actRig) rows(t *testing.T, kind string) []Event {
	t.Helper()
	events, err := g.s.TaskEvents(context.Background(), "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			out = append(out, events[i])
		}
	}
	return out
}

func (g actRig) card(t *testing.T) Task {
	t.Helper()
	card, _, err := g.s.Task(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return card
}

var oneSession = Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}

// tripWaits writes on t1's case the step of rung 0 a trip under the cases
// gate writes on the case it waits on: its run reads an extend's raise
// from the card's row (runGrants).
func (g actRig) tripWaits(t *testing.T) {
	t.Helper()
	if _, err := g.s.AppendCaseStep(context.Background(), CaseStep{CaseID: g.c.ID, Rung: 0, Actor: FactoryDecider, Found: "no resolver fits",
		Outcome: StepHandedUp}); err != nil {
		t.Fatal(err)
	}
}

// posting is a Post that answers comment id once before ran.
func posting(id int64, before func()) func(context.Context) (int64, error) {
	return func(context.Context) (int64, error) {
		if before != nil {
			before()
		}
		return id, nil
	}
}

// The human's extend of one session on the live case a trip waits on
// resolves it and raises the card's sessions in one transaction: the case reads resolved
// by sebastian on rung 3 with extend, its step says what was drawn, and
// the card_extended row is a pool draw of rung 3 naming the case. A second
// answer finds the case ended, names the first, and draws nothing.
func TestActResolvesAndDrawsInOneTransaction(t *testing.T) {
	g := newActRig(t)
	g.tripWaits(t)
	ctx := context.Background()
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Post: posting(77, nil), Res: oneSession})
	if err != nil {
		t.Fatal(err)
	}
	if got.Drawn != (policy.Draw{Sessions: 1}) || got.Comment != 77 || got.Case.ID != g.c.ID {
		t.Fatalf("result = %+v, want case %d, 1 session drawn, comment 77", got, g.c.ID)
	}
	c := caseByID(t, g.s, g.c.ID)
	want := &CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend, Args: map[string]any{"sessions": 1.0, "comment": 77.0}}
	if c.Status != CaseResolved || !reflect.DeepEqual(c.Resolution, want) {
		t.Fatalf("case = %s, %+v; want resolved by %+v", c.Status, c.Resolution, want)
	}
	if card := g.card(t); card.Usage.RaisedSessions != 1 {
		t.Fatalf("raised sessions = %d, want 1", card.Usage.RaisedSessions)
	}
	extended := g.rows(t, EventCardExtended)
	if len(extended) != 1 {
		t.Fatalf("card_extended rows = %+v, want one", extended)
	}
	d := extended[0].Detail
	if d["pool"] != true || d["rung"] != 3.0 || d["case_id"] != float64(g.c.ID) || d["sessions"] != 1.0 || extended[0].Actor != "sebastian" {
		t.Fatalf("the draw row = %+v, want a pool draw of 1 session by sebastian on rung 3 for case %d", extended[0], g.c.ID)
	}
	steps := caseStepsOf(t, g.s, g.c.ID)
	last := steps[len(steps)-1]
	if last.Rung != 3 || last.Actor != "sebastian" || last.Decision != ActExtend || last.Outcome != StepResolved ||
		len(last.Did) != 1 || last.Did[0].Action != ActExtend || last.Did[0].Result != "Added 1 session from the pool." {
		t.Fatalf("the step = %+v, want sebastian's extend on rung 3, resolved, that drew 1 session from the pool", last)
	}

	_, err = g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "ana", Rung: 3, Res: oneSession})
	var ended ErrCaseEnded
	at := c.ResolvedAt.UTC().Format("15:04")
	if !errors.As(err, &ended) || err.Error() != "the session cap on #12 was resolved by sebastian with extend at "+at+" UTC" {
		t.Fatalf("a second answer = %v, want the case resolved by sebastian with extend at %s UTC", err, at)
	}
	if card := g.card(t); card.Usage.RaisedSessions != 1 || len(g.rows(t, EventCardExtended)) != 1 {
		t.Fatalf("after the second answer: raised sessions %d, %d draw rows; want 1 and 1", card.Usage.RaisedSessions, len(g.rows(t, EventCardExtended)))
	}
}

// Another card drew the whole pool this month: 30 sessions, 30 hours and
// 60 EUR. Rung 2's extend of one session is refused naming the pool. The
// human's goes through, and the month reads 31 sessions drawn, past the
// pool's 30.
func TestActRefusesARungsDrawPastThePool(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "t0", Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 11, Track: policy.TrackDirect, BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t0", Kind: EventCardExtended, Actor: ChatUser, Via: ViaChat,
		Detail: map[string]any{"pool": true, "rung": 2, "sessions": 30, "agent_time_s": 108000, "amount_eur": 60}}); err != nil {
		t.Fatal(err)
	}
	_, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: ChatUser, Rung: 2, Res: oneSession})
	var refused ErrActRefused
	const pool = "the escalation pool leaves 0 sessions, 0 seconds of agent time and 0.00 EUR this month: only the human draws more"
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), pool+", and rung 2 may take ") {
		t.Fatalf("rung 2's extend = %v, want the pool's refusal", err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseAtHuman {
		t.Fatalf("after the refusal the case is %s, want at_human", c.Status)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Res: oneSession}); err != nil {
		t.Fatalf("the human's extend past the pool: %v", err)
	}
	drawn, err := g.s.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if want := (policy.Draw{Sessions: 31, AgentTime: 30 * time.Hour, EUR: 60}); err != nil || drawn != want {
		t.Fatalf("PoolDrawn = %+v, %v; want %+v, past the pool", drawn, err, want)
	}
}

// A refused answer and a note call no Post. With no Post, Act posts
// factoryd's comment on the card's issue, naming the human and the action.
// case_resolved carries the case, the rung, the answer and that comment,
// and answer_whole the comment. The answer row is a session_granted: its
// draw holds a session. A caller's Post is called once, its comment is not
// posted again by factoryd and rides in both signals.
func TestActSignalsTheRunAndPostsTheComment(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	posted := 0
	count := posting(90, func() { posted++ })
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Post: count, Res: Resolution{Action: ActRouteApproval}}); err == nil {
		t.Fatal("the human's route_approval went through, want it refused")
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Post: count, Res: Resolution{Action: ActNote, Note: "wait"}}); err != nil {
		t.Fatal(err)
	}
	if posted != 0 || len(g.forge.comments) != 0 {
		t.Fatalf("Post called %d times, forgejo holds %q; want none for a refusal and a note", posted, g.forge.comments)
	}
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: oneSession})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"factoryd: sebastian answered with extend: 1 session from the pool."}; got.Comment != 501 || !reflect.DeepEqual(g.forge.comments, want) {
		t.Fatalf("comment %d, forgejo holds %q; want 501 and %q", got.Comment, g.forge.comments, want)
	}
	want := []reopenCall{
		{"t1", SignalCaseResolved, TaskSignal{Author: "sebastian", Comment: 501,
			Case: &CaseSignal{CaseID: g.c.ID, Rung: 3, By: "sebastian", Res: oneSession, Comment: 501}}},
		{"t1", SignalAnswerWhole, TaskSignal{Author: "sebastian", Comment: 501}},
	}
	if !reflect.DeepEqual(g.signals.got, want) {
		t.Fatalf("signals = %+v, want %+v", g.signals.got, want)
	}
	answers := g.rows(t, EventSessionGranted)
	if len(answers) != 1 || answers[0].Detail["action"] != ActExtend || answers[0].Detail["case_id"] != float64(g.c.ID) ||
		answers[0].Detail["comment"] != 501.0 || answers[0].Actor != "sebastian" || answers[0].Via != ViaConsole {
		t.Fatalf("answer rows = %+v, want sebastian's session_granted via console naming extend, the case and comment 501", answers)
	}

	g.signals.got = nil
	second, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseAgentEscalate, StateKey: "ask",
		Status: CaseAtHuman, Problem: "Which file?", CommentID: 71})
	if err != nil {
		t.Fatal(err)
	}
	cont := Resolution{Action: ActContinue, Note: "the one under cmd"}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: second.ID, By: "sebastian", Rung: 3, Post: posting(88, nil), Res: cont}); err != nil {
		t.Fatal(err)
	}
	if len(g.forge.comments) != 1 {
		t.Fatalf("forgejo holds %q, want the caller's comment not posted again", g.forge.comments)
	}
	want = []reopenCall{
		{"t1", SignalCaseResolved, TaskSignal{Author: "sebastian", Comment: 88, Case: &CaseSignal{CaseID: second.ID, Rung: 3, By: "sebastian", Res: cont, Comment: 88}}},
		{"t1", SignalAnswerWhole, TaskSignal{Author: "sebastian", Comment: 88}},
	}
	if !reflect.DeepEqual(g.signals.got, want) {
		t.Fatalf("signals = %+v, want %+v", g.signals.got, want)
	}
	if answers := g.rows(t, EventQuestionAnswered); len(answers) != 1 || answers[0].Detail["action"] != ActContinue || answers[0].Detail["comment"] != 88.0 {
		t.Fatalf("answer rows = %+v, want the continue's question_answered naming comment 88", answers)
	}
}

// A note records a step and resolves nothing: the case stays at the human,
// no comment, no answer row, no signal.
func TestActNoteKeepsTheCaseOpen(t *testing.T) {
	g := newActRig(t)
	got, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActNote, Note: "keep waiting for the image"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseAtHuman || c.Resolution != nil || got.Case.Status != CaseAtHuman {
		t.Fatalf("case = %+v, result %+v; want it at the human, unresolved", c, got.Case)
	}
	steps := caseStepsOf(t, g.s, g.c.ID)
	if len(steps) != 1 || steps[0].Rung != 3 || steps[0].Actor != "sebastian" || steps[0].Decision != ActNote ||
		steps[0].Grounds != "keep waiting for the image" || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want sebastian's note in progress", steps)
	}
	if len(g.forge.comments) != 0 || len(g.signals.got) != 0 || len(g.rows(t, EventQuestionAnswered)) != 0 {
		t.Fatalf("comments %q, signals %+v: want none", g.forge.comments, g.signals.got)
	}
}

// The run's end of a wait withdraws the case with its reason. A case that
// ended already, and no case at all, are no error.
func TestEndCaseWithdrawsWithTheReason(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	const why = "the wait ended on its own: the month cap turned"
	if err := g.r.EndCase(ctx, g.c.ID, why); err != nil {
		t.Fatal(err)
	}
	c := caseByID(t, g.s, g.c.ID)
	if c.Status != CaseWithdrawn || c.Resolution == nil || c.Resolution.Reason != why || c.Resolution.By != FactoryDecider {
		t.Fatalf("case = %s, %+v; want withdrawn by factoryd for %q", c.Status, c.Resolution, why)
	}
	if err := g.r.EndCase(ctx, g.c.ID, "again"); err != nil {
		t.Fatalf("a second end = %v, want none", err)
	}
	if err := g.r.EndCase(ctx, 0, why); err != nil {
		t.Fatalf("no case = %v, want none", err)
	}
}

// A fund of 3 EUR on a budget of 3 passes the policy's ceiling of 5. The
// human raises the ceiling by 2 EUR and funds 4 EUR beside it: the raise is
// written first, so the fund lands on 7 EUR at the ceiling of 7. The
// ceiling's row draws nothing from the pool, the fund's 4 EUR does.
func TestActRaisesTheCeilingThenFundsInOneTransaction(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	res := Resolution{Action: ActRaiseCeiling, Args: map[string]any{"eur": 2.0}, Fund: &policy.Draw{EUR: 2}}
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Res: Resolution{
		Action: ActFund, Args: map[string]any{"eur": 3.0}}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "3.00 EUR more takes the budget of 3.00 EUR past the card's ceiling of 5.00 EUR: raise the ceiling first") {
		t.Fatalf("a fund of 3 = %+v, %v; want the ceiling's refusal", got, err)
	}
	res.Fund.EUR = 4
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Res: res}); err != nil {
		t.Fatal(err)
	}
	card := g.card(t)
	if card.CeilingEUR != 7 || card.BudgetEUR != 7 {
		t.Fatalf("ceiling %v, budget %v; want 7 and 7", card.CeilingEUR, card.BudgetEUR)
	}
	funded := g.rows(t, EventCardFunded)
	if len(funded) != 2 || funded[0].Detail["ceiling_eur"] != 7.0 || funded[0].Detail["pool"] != nil || funded[0].Detail["case_id"] != float64(g.c.ID) ||
		funded[1].Detail["amount_eur"] != 4.0 || funded[1].Detail["pool"] != true {
		t.Fatalf("card_funded rows = %+v, want the ceiling of 7 off the pool, then 4 EUR from it", funded)
	}
	if drawn, err := g.s.PoolDrawn(ctx, "demo-app/demo-app", time.Now()); err != nil || drawn != (policy.Draw{EUR: 4}) {
		t.Fatalf("PoolDrawn = %+v, %v; want 4 EUR", drawn, err)
	}
}

// The human's close of a card with an issue posts the note and closes the
// issue as factoryd's recorded close, which the webhook then ends the card
// on. The case reads resolved with close.
func TestActClosesTheIssueAsFactory(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose, Note: "not worth it"}}); err != nil {
		t.Fatal(err)
	}
	if !g.forge.closed || len(g.forge.comments) != 1 || !strings.HasPrefix(g.forge.comments[0], "factoryd: sebastian answered with close.") ||
		!strings.Contains(g.forge.comments[0], "not worth it") {
		t.Fatalf("closed %v, comments %q; want the issue closed after one comment naming sebastian and the note", g.forge.closed, g.forge.comments)
	}
	events, err := g.s.IntakeEvents(ctx, "t1")
	if err != nil || len(events) != 1 || events[0].Kind != IntakeFactoryClosed || events[0].CommentID != 12 {
		t.Fatalf("intake rows = %+v, %v; want factoryd's recorded close of issue 12", events, err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseResolved || c.Resolution.Action != ActClose {
		t.Fatalf("case = %s, %+v; want resolved with close", c.Status, c.Resolution)
	}
}

// The human's comment comes after the close: a close Forgejo refuses
// leaves the case open with nothing posted, and once the close goes
// through, Post runs on the closed issue and factoryd posts no comment of
// its own.
func TestActPostsTheHumansCommentAfterTheClose(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	var closedAtPost []bool
	post := func(context.Context) (int64, error) {
		g.forge.mu.Lock()
		defer g.forge.mu.Unlock()
		closedAtPost = append(closedAtPost, g.forge.closed)
		return 91, nil
	}
	in := ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose, Note: "not worth it"}, Post: post}
	g.forge.closeFails = true
	if _, err := g.r.Act(ctx, in); err == nil {
		t.Fatal("the close Forgejo refused went through")
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseAtHuman || len(closedAtPost) != 0 {
		t.Fatalf("case %s, Post called %d times; want the case open and no Post", c.Status, len(closedAtPost))
	}
	g.forge.closeFails = false
	got, err := g.r.Act(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(closedAtPost, []bool{true}) || got.Comment != 91 || len(g.forge.comments) != 0 || caseByID(t, g.s, g.c.ID).Status != CaseResolved {
		t.Fatalf("closed at Post %v, comment %d, factoryd's comments %q; want one Post after the close, comment 91, none of factoryd's, the case resolved",
			closedAtPost, got.Comment, g.forge.comments)
	}
}

// A withdrawal with no note gives the reason who withdrew it, so its end
// never reads as a withdrawal for no reason. Withdraw is the factory's
// own: factoryd withdraws on rung 0.
func TestActWithdrawalWithNoNoteNamesWhoWithdrew(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: FactoryDecider, Rung: 0, Res: Resolution{Action: ActWithdraw}}); err != nil {
		t.Fatal(err)
	}
	c := caseByID(t, g.s, g.c.ID)
	want := fmt.Sprintf("the session cap on #12 was withdrawn at %s UTC: withdrawn by %s", c.ResolvedAt.UTC().Format("15:04"), FactoryDecider)
	if err := (ErrCaseEnded{Case: c, Card: "#12"}); err.Error() != want {
		t.Fatalf("the ended case reads %q, want %q", err.Error(), want)
	}
}

// The webhook's copy of the human's comment may resolve the case between
// Post and the transaction: Act's answer on that very comment resolves it
// again with its action and draw. A case the copy of another comment
// resolved stays ended as that answer, and the run hears nothing more.
func TestActTakesOverTheWebhooksCopyOfItsComment(t *testing.T) {
	g := newActRig(t)
	g.tripWaits(t)
	ctx := context.Background()
	webhook := func(comment int64) func() {
		return func() {
			if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventQuestionAnswered,
				Actor: "sebastian", Via: ViaWebhook, Detail: map[string]any{"comment": comment}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Post: posting(77, webhook(77)), Res: oneSession}); err != nil {
		t.Fatalf("the answer on comment 77: %v", err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseResolved || c.Resolution.Action != ActExtend || g.card(t).Usage.RaisedSessions != 1 {
		t.Fatalf("case = %s, %+v, raised %d; want resolved with extend and 1 session raised", c.Status, c.Resolution, g.card(t).Usage.RaisedSessions)
	}
	second, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseAgentEscalate, StateKey: "ask",
		Status: CaseAtHuman, Problem: "Which file?"})
	if err != nil {
		t.Fatal(err)
	}
	cont := Resolution{Action: ActContinue, Note: "the one under cmd"}
	_, err = g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: second.ID, By: "sebastian", Rung: 3, Post: posting(79, webhook(78)), Res: cont})
	var ended ErrCaseEnded
	if !errors.As(err, &ended) || ended.Case.Resolution.Action != caseAnswer {
		t.Fatalf("an answer on comment 79 after the copy of 78 resolved its case = %v, want the case ended with answer", err)
	}
	if c := caseByID(t, g.s, second.ID); c.Resolution.Action != caseAnswer || len(g.signals.got) != 2 {
		t.Fatalf("the second case = %+v, signals %+v; want the webhook copy's answer kept and only the first answer's two signals", c.Resolution, g.signals.got)
	}
}

// factoryd withdraws a case the card outgrew on rung 0, withdraw being
// the factory's own: the case reads withdrawn by factoryd with the
// reason, its step says so, and no comment, answer row or signal follows.
func TestActWithdrawsTheCase(t *testing.T) {
	g := newActRig(t)
	if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: g.c.ID, By: FactoryDecider, Rung: 0,
		Res: Resolution{Action: ActWithdraw, Note: "the card moved past its cap"}}); err != nil {
		t.Fatal(err)
	}
	c := caseByID(t, g.s, g.c.ID)
	want := &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWithdraw, Reason: "the card moved past its cap"}
	if c.Status != CaseWithdrawn || !reflect.DeepEqual(c.Resolution, want) {
		t.Fatalf("case = %s, %+v; want withdrawn by %+v", c.Status, c.Resolution, want)
	}
	if steps := caseStepsOf(t, g.s, g.c.ID); len(steps) != 1 || steps[0].Outcome != StepWithdrawn || steps[0].Rung != 0 {
		t.Fatalf("steps = %+v, want factoryd's withdrawn step", steps)
	}
	if len(g.forge.comments) != 0 || len(g.signals.got) != 0 || len(g.rows(t, EventQuestionAnswered)) != 0 {
		t.Fatalf("comments %q, signals %+v: want none", g.forge.comments, g.signals.got)
	}
}

// One session is left in the project's pool and two cards' cases are
// answered by the chat at the same moment, each drawing one: one lands, the
// other is refused naming the pool, and the month reads 30 sessions drawn.
func TestActDrawsThePoolOnceUnderTwoAnswers(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 14, Track: policy.TrackDirect, Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	other, _, err := g.s.OpenCase(ctx, Case{TaskID: "t2", Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "cap",
		Status: CaseAtHuman, Problem: "Card #14 used its sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t2", Kind: EventCardExtended, Actor: ChatUser, Via: ViaChat,
		Detail: map[string]any{"pool": true, "rung": 2, "sessions": 29}}); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, in := range []ActInput{{TaskID: "t1", CaseID: g.c.ID}, {TaskID: "t2", CaseID: other.ID}} {
		in.By, in.Rung, in.Res = ChatUser, 2, oneSession
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = g.r.Act(ctx, in)
		}()
	}
	wg.Wait()
	var refused ErrActRefused
	landed := slices.IndexFunc(errs, func(err error) bool { return err == nil })
	if landed < 0 || !errors.As(errs[1-landed], &refused) || !strings.Contains(errs[1-landed].Error(), "the escalation pool leaves 0 sessions") {
		t.Fatalf("errors = %v, want one answer to land and the other refused by the pool", errs)
	}
	if drawn, err := g.s.PoolDrawn(ctx, "demo-app/demo-app", time.Now()); err != nil || drawn.Sessions != 30 {
		t.Fatalf("PoolDrawn = %+v, %v; want 30 sessions", drawn, err)
	}
}

// The resolution's transaction weighs the draw against the pool again
// under the project's lock: with the pool's 30 sessions drawn, a draw of
// one more the facts let through is refused, the case stays live and
// nothing is drawn. The human's draw, with no bound, goes through.
func TestResolveCaseWeighsThePoolUnderItsLock(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", Kind: EventCardExtended, Actor: ChatUser, Via: ViaChat,
		Detail: map[string]any{"pool": true, "rung": 2, "sessions": 30}}); err != nil {
		t.Fatal(err)
	}
	draw := CaseDraw{Rung: 2, Extend: policy.Draw{Sessions: 1}, Pool: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60},
		Event: Event{Repository: "demo-app/demo-app", IssueNumber: 12, Actor: ChatUser, Via: ViaChat, Detail: map[string]any{"pool": true, "rung": 2}}}
	res := CaseResolution{By: ChatUser, Rung: 2, Action: ActExtend}
	var spent *DrawBoundError
	if err := g.s.ResolveCase(ctx, g.c.ID, res, &draw, &CaseStep{Rung: 2, Actor: ChatUser}, nil); !errors.As(err, &spent) || spent.Card ||
		spent.Left != (policy.Draw{AgentTime: 30 * time.Hour, EUR: 60}) {
		t.Fatalf("ResolveCase past the pool = %v, want the pool's DrawBoundError leaving 0 sessions", err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseAtHuman || g.card(t).Usage.RaisedSessions != 0 || len(caseStepsOf(t, g.s, g.c.ID)) != 0 {
		t.Fatalf("case %s, raised %d: want it live, nothing raised and no step", c.Status, g.card(t).Usage.RaisedSessions)
	}
	draw.Rung, res.By, res.Rung = 3, "sebastian", 3
	if err := g.s.ResolveCase(ctx, g.c.ID, res, &draw, nil, nil); err != nil || g.card(t).Usage.RaisedSessions != 1 {
		t.Fatalf("the human's draw = %v, raised %d; want it through", err, g.card(t).Usage.RaisedSessions)
	}
}

// The card's draw bound is weighed in the transaction too: with rung 1's
// draws of 3 sessions on t1, the whole bound of a large card, rung 1's
// draw of one more is refused naming the bound and the case stays live.
// Rung 2 draws past the card's bound, within the pool.
func TestResolveCaseWeighsTheCardsDrawBoundUnderTheLock(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", Kind: EventCardExtended, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"pool": true, "rung": 1, "sessions": 3}}); err != nil {
		t.Fatal(err)
	}
	draw := CaseDraw{Rung: 1, Extend: policy.Draw{Sessions: 1}, CardBound: policy.Draw{Sessions: 3, AgentTime: 90 * time.Minute, EUR: 6},
		Pool:  policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60},
		Event: Event{Repository: "demo-app/demo-app", IssueNumber: 12, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"pool": true, "rung": 1}}}
	res := CaseResolution{By: FactoryDecider, Rung: 1, Action: ActExtend}
	var bound *DrawBoundError
	err := g.s.ResolveCase(ctx, g.c.ID, res, &draw, nil, nil)
	if !errors.As(err, &bound) || !bound.Card || err.Error() != "the card's draw bound leaves 0 sessions, 90 minutes of agent time and 6.00 EUR: rung 2 or the human may draw more" {
		t.Fatalf("ResolveCase past the card's bound = %v, want the card's DrawBoundError", err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseAtHuman || g.card(t).Usage.RaisedSessions != 0 {
		t.Fatalf("case %s, raised %d: want it live and nothing raised", c.Status, g.card(t).Usage.RaisedSessions)
	}
	draw.Rung, res.Rung = 2, 2
	if err := g.s.ResolveCase(ctx, g.c.ID, res, &draw, nil, nil); err != nil || g.card(t).Usage.RaisedSessions != 1 {
		t.Fatalf("rung 2's draw = %v, raised %d; want it through", err, g.card(t).Usage.RaisedSessions)
	}
}

// A draw that fails in the resolution's transaction, a fund of 10 EUR past
// the policy's ceiling of 5 on a budget of 3, rolls the case's end back:
// it stays at the human with no step, and nothing is drawn.
func TestResolveCaseLeavesTheCaseLiveWhenItsDrawFails(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	draw := CaseDraw{Rung: 3, FundEUR: 10, PolicyCeiling: 5, Event: Event{Repository: "demo-app/demo-app", IssueNumber: 12, Actor: "sebastian", Via: ViaConsole,
		Detail: map[string]any{"pool": true, "rung": 3}}}
	var capped *CardCapError
	if err := g.s.ResolveCase(ctx, g.c.ID, CaseResolution{By: "sebastian", Rung: 3, Action: ActFund}, &draw, &CaseStep{Rung: 3, Actor: "sebastian"}, nil); !errors.As(err, &capped) {
		t.Fatalf("ResolveCase = %v, want CardCapError", err)
	}
	c := caseByID(t, g.s, g.c.ID)
	if c.Status != CaseAtHuman || c.Resolution != nil || g.card(t).BudgetEUR != 3 || len(g.rows(t, EventCardFunded)) != 0 || len(caseStepsOf(t, g.s, g.c.ID)) != 0 {
		t.Fatalf("case %s, %+v, budget %v: want it at the human, unresolved, 3 EUR, no row and no step", c.Status, c.Resolution, g.card(t).BudgetEUR)
	}
}

// A human card's case takes a value of its question's choices: Act records
// it, resolves the case and runs nothing, and no run is signalled. A value
// the question does not offer is refused with the choices.
func TestActTakesAHumanCardsChoice(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 13, BudgetEUR: 0}); err != nil {
		t.Fatal(err)
	}
	q := HumanQuestion{Ask: "Roll the release?", Choices: []QuestionChoice{{Value: AnswerDismiss, Label: "Dismiss"}, {Value: AnswerCheckAgain, Label: "Check again"}}}
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "h1", Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: "roll", Status: CaseAtHuman, Problem: q.Ask})
	if err != nil {
		t.Fatal(err)
	}
	askOwn(t, g.s, "h1", c.ID, q.Choices...)
	_, err = g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: "deploy"}})
	var refused ErrActRefused
	if want := "card #13 takes one of its choices, with your note beside it: Dismiss (dismiss), Check again (check_again)"; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("a value it does not offer = %v, want %q", err, want)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerDismiss}}); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution.Action != AnswerDismiss {
		t.Fatalf("case = %s, %+v; want resolved with %s", got.Status, got.Resolution, AnswerDismiss)
	}
	events, err := g.s.TaskEventsOfKind(ctx, "h1", EventQuestionAnswered, 10)
	if err != nil || len(events) != 1 || events[0].Detail["value"] != AnswerDismiss {
		t.Fatalf("h1's rows = %+v, %v; want the answer row with the value", events, err)
	}
	if len(g.signals.got) != 0 || len(g.forge.comments) != 0 {
		t.Fatalf("signals %+v, comments %q; want none for a human card", g.signals.got, g.forge.comments)
	}
}

// A card with two live cases takes a choice on the case whose own ask offers
// it: a value only the older case's ask offers is taken there and refused on
// the newer case, though the newer ask is the card's newest.
func TestActReadsTheChoicesOfTheCasesOwnAsk(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h2", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 14}); err != nil {
		t.Fatal(err)
	}
	open := func(key string, offers ...QuestionChoice) Case {
		c, _, err := g.s.OpenCase(ctx, Case{TaskID: "h2", Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: key, Status: CaseAtHuman, Problem: key})
		if err != nil {
			t.Fatal(err)
		}
		askOwn(t, g.s, "h2", c.ID, offers...)
		return c
	}
	older := open("older", QuestionChoice{Value: AnswerDismiss, Label: "Dismiss"}, QuestionChoice{Value: AnswerCheckAgain, Label: "Check again"})
	newer := open("newer", QuestionChoice{Value: AnswerDismiss, Label: "Dismiss"})
	_, err := g.r.Act(ctx, ActInput{TaskID: "h2", CaseID: newer.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerCheckAgain}})
	var refused ErrActRefused
	if want := "card #14 takes one of its choices, with your note beside it: Dismiss (dismiss)"; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("the newer case taking the older's choice = %v, want %q", err, want)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h2", CaseID: older.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerCheckAgain}}); err != nil {
		t.Fatalf("the older case taking its own choice: %v", err)
	}
	if got := caseByID(t, g.s, older.ID); got.Status != CaseResolved || got.Resolution.Action != AnswerCheckAgain {
		t.Fatalf("older case = %s, %+v; want resolved with %s", got.Status, got.Resolution, AnswerCheckAgain)
	}
	if got := caseByID(t, g.s, newer.ID); got.Status != CaseAtHuman {
		t.Fatalf("newer case = %s; want it still at the human", got.Status)
	}
}

// A human card's close is a choice like any other: Act records it and
// resolves the case, and leaves the issue to the console, which closes it
// as the human. Nothing goes to Forgejo.
func TestActLeavesAHumanCardsCloseToTheConsole(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 13}); err != nil {
		t.Fatal(err)
	}
	q := HumanQuestion{Ask: "Rotate the key?", Choices: []QuestionChoice{{Value: AnswerGo, Label: "Have the factory do it"}, {Value: AnswerClose, Label: "Done"}}}
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "h1", Kind: IntakeEscalation, Author: "factoryd", Body: q.Ask, Question: &q, Choices: q.Labels()}); err != nil {
		t.Fatal(err)
	}
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "h1", Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: "key", Status: CaseAtHuman, Problem: q.Ask})
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerClose, Note: "Done"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution.Action != AnswerClose || len(out.Did) != 0 {
		t.Fatalf("case = %s, %+v, did %+v; want resolved with close and nothing run", got.Status, got.Resolution, out.Did)
	}
	if g.forge.closed || len(g.forge.comments) != 0 || len(g.signals.got) != 0 {
		t.Fatalf("closed %v, comments %q, signals %+v; want no Forgejo call and no signal", g.forge.closed, g.forge.comments, g.signals.got)
	}
}

// The keys the caller hands beside an answer ride every row Act writes for
// it, the draw's and the answer's, so a chat token's turn names its token
// on each; a key of Act's own is never replaced.
func TestActCarriesTheCallersDetailOnEveryRow(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	token := map[string]any{"id": 3, "name": "laptop"}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Via: ViaChat, Rung: 3,
		Res:    Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}, Note: "go on"},
		Detail: map[string]any{"token": token, "rung": "the caller's"}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": 3.0, "name": "laptop"}
	funded, answered := g.rows(t, EventCardFunded), g.rows(t, EventQuestionAnswered)
	if len(funded) != 1 || !reflect.DeepEqual(funded[0].Detail["token"], want) || funded[0].Detail["rung"] != 3.0 {
		t.Fatalf("card_funded rows = %+v; want one with the token and rung 3", funded)
	}
	if len(answered) != 1 || !reflect.DeepEqual(answered[0].Detail["token"], want) {
		t.Fatalf("question_answered rows = %+v; want one with the token", answered)
	}
}

// An extend of 200 lines alone resolves the case and raises the card's
// line bound by 200: one card_extended row of 200 lines and no session,
// agent time or money, nothing drawn from the pool, and the step says
// what it raised.
func TestActExtendsTheLinesAlone(t *testing.T) {
	g := newActRig(t)
	got, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActExtend, Args: map[string]any{"lines": 200}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseAction{{Action: ActExtend, Args: map[string]any{"lines": 200}, Result: "Added 200 lines to its line bound."}}
	if !got.Drawn.IsZero() || !reflect.DeepEqual(got.Did, want) || got.Case.Status != CaseResolved {
		t.Fatalf("result = %+v, want case resolved, nothing drawn and the step %+v", got, want)
	}
	if card := g.card(t); card.Usage.ExtraLines != 200 || card.Usage.RaisedSessions != 0 {
		t.Fatalf("extra lines %d, raised sessions %d; want 200 and 0", card.Usage.ExtraLines, card.Usage.RaisedSessions)
	}
	rows := g.rows(t, EventCardExtended)
	if len(rows) != 1 || rows[0].Detail["lines"] != 200.0 || rows[0].Detail["sessions"] != 0.0 || rows[0].Detail["agent_time_s"] != 0.0 ||
		rows[0].Detail["amount_eur"] != 0.0 {
		t.Fatalf("card_extended rows = %+v, want one of 200 lines and nothing else", rows)
	}
}

// Closing a human card is open to the human whatever its question offers,
// as its Close button is: Act resolves its case with close on a question
// whose choices hold no close, and on a card that asks no question.
func TestActTakesAHumanCardsCloseItsQuestionDoesNotOffer(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	for _, tc := range []struct {
		id    string
		issue int64
		q     *HumanQuestion
	}{
		{"h1", 13, &HumanQuestion{Ask: "Roll the release?", Choices: []QuestionChoice{{Value: AnswerDismiss, Label: "Dismiss"}}}},
		{"h2", 14, nil},
	} {
		if err := g.s.CreateTask(ctx, Task{ID: tc.id, Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app",
			IssueNumber: tc.issue}); err != nil {
			t.Fatal(err)
		}
		if tc.q != nil {
			if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: tc.id, Kind: IntakeEscalation, Author: "factoryd", Body: tc.q.Ask,
				Question: tc.q, Choices: tc.q.Labels()}); err != nil {
				t.Fatal(err)
			}
		}
		c, _, err := g.s.OpenCase(ctx, Case{TaskID: tc.id, Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: tc.id,
			Status: CaseAtHuman, Problem: "a person's work"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.r.Act(ctx, ActInput{TaskID: tc.id, CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose, Note: "done by hand"}}); err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution.Action != ActClose {
			t.Fatalf("%s: case = %s, %+v; want resolved with close", tc.id, got.Status, got.Resolution)
		}
	}
	if g.forge.closed || len(g.forge.comments) != 0 {
		t.Fatalf("closed %v, comments %q; want no Forgejo call", g.forge.closed, g.forge.comments)
	}
}

// The grant at a check-in share is one more share of the check-in's
// budget: the extend raises the card's extra check-in share by 1 and
// draws nothing, its card_extended row names the share, and the run's
// usage snapshot, which never holds it, does not write it away.
func TestActGrantsACheckinShare(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseCheckinShare, StateKey: "share",
		Status: CaseAtHuman, Problem: "check-in: spend_above_policy: the check-in's share of the budget is spent"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActExtend, Args: map[string]any{"checkin_shares": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseAction{{Action: ActExtend, Args: map[string]any{"checkin_shares": 1}, Result: "Added 1 more check-in share."}}
	if !got.Drawn.IsZero() || !reflect.DeepEqual(got.Did, want) {
		t.Fatalf("result = %+v, want nothing drawn and %+v", got, want)
	}
	if text := `The check-in share on #12 ("check-in: spend_above_policy: the check-in's share of the budget is spent") is resolved with extend: 1 more check-in share.`; got.Text != text {
		t.Fatalf("text = %q, want %q", got.Text, text)
	}
	if card := g.card(t); card.Usage.ExtraCheckinShare != 1 {
		t.Fatalf("extra check-in share = %d, want 1", card.Usage.ExtraCheckinShare)
	}
	rows := g.rows(t, EventCardExtended)
	if len(rows) != 1 || rows[0].Detail["checkin_shares"] != 1.0 || rows[0].Detail["sessions"] != 0.0 {
		t.Fatalf("card_extended rows = %+v, want one of 1 check-in share", rows)
	}
	if err := g.s.SetTaskUsage(ctx, "t1", TaskUsage{Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if card := g.card(t); card.Usage.ExtraCheckinShare != 1 || card.Usage.Attempts != 2 {
		t.Fatalf("after the run's snapshot: extra check-in share %d, attempts %d; want 1 and 2", card.Usage.ExtraCheckinShare, card.Usage.Attempts)
	}
}

// A caller's own "comment" key never reaches the case: the comment the
// resolution names is the one the answer posted, and an answer that
// posted none names none.
func TestActDropsTheCallersComment(t *testing.T) {
	g := newActRig(t)
	if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1, "comment": 99}}}); err != nil {
		t.Fatal(err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Resolution == nil || c.Resolution.Args["comment"] != nil {
		t.Fatalf("resolution = %+v, want no comment of the caller's", c.Resolution)
	}
}

// A grant on a card that ended, with no case, is refused by the table
// naming the end: nothing is drawn, no row is written and the card's
// sessions stay.
func TestActRefusesAnExtendOnAnEndedCard(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "done", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 14, Track: policy.TrackDirect}); err != nil {
		t.Fatal(err)
	}
	_, err := g.r.Act(ctx, ActInput{TaskID: "t2", By: "sebastian", Rung: 3, Res: oneSession})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "this card ended done: an extend buys nothing, file a new card") {
		t.Fatalf("an extend on a done card = %v, want the refusal naming its end", err)
	}
	card, _, err := g.s.Task(ctx, "t2")
	if err != nil || card.Usage.RaisedSessions != 0 {
		t.Fatalf("t2 = %+v, %v; want no session raised", card.Usage, err)
	}
	if events, err := g.s.TaskEvents(ctx, "t2", 10); err != nil || len(events) != 0 {
		t.Fatalf("t2's rows = %+v, %v; want none", events, err)
	}
}

// A draw of nothing is not weighed: an extend of lines or of a check-in
// share on rung 2 goes through on a pool the month has overdrawn, where a
// session is refused.
func TestActWeighsNoDrawOfNothing(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t0", Kind: EventCardFunded, Actor: "sebastian", Via: ViaConsole,
		Detail: map[string]any{"pool": true, "rung": 3, "amount_eur": 61.0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", By: ChatUser, Rung: 2, CaseID: g.c.ID, Res: oneSession}); err == nil {
		t.Fatal("rung 2's session past the overdrawn pool went through")
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: ChatUser, Rung: 2, Res: Resolution{Action: ActExtend, Args: map[string]any{"lines": 100}}}); err != nil {
		t.Fatalf("rung 2's lines on the overdrawn pool: %v", err)
	}
	if card := g.card(t); card.Usage.ExtraLines != 100 {
		t.Fatalf("extra lines = %d, want 100", card.Usage.ExtraLines)
	}
}

// An extend's step and its answer's text name every part it raised: a
// session from the pool and the line bound beside it.
func TestActStepNamesEveryPartOfAnExtend(t *testing.T) {
	g := newActRig(t)
	got, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1, "lines": 100}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseAction{{Action: ActExtend, Args: map[string]any{"sessions": 1, "lines": 100}, Result: "Added 1 session from the pool and 100 lines to its line bound."}}
	if !reflect.DeepEqual(got.Did, want) {
		t.Fatalf("did = %+v, want %+v", got.Did, want)
	}
	if text := `The session cap on #12 ("Card #12 used its sessions") is resolved with extend: 1 session from the pool and 100 lines to its line bound.`; got.Text != text {
		t.Fatalf("text = %q, want %q", got.Text, text)
	}
}

// A check-in's spend stop names its stop in the case's facts, from the
// trip, not from its reason: the repeat of a share stop, whose ask names
// no share, still opens as the share's.
func TestTripCaseKeepsTheCheckinStop(t *testing.T) {
	g := newActRig(t)
	repeat := checkinSpendAsk + "The restart produced no new commit, the head is still c1. What should the agent do?"
	c, _, err := g.r.tripCase(context.Background(), g.card(t), CaseFacts{}, TripInput{TaskID: "t1", Reason: repeat, CheckinStop: CheckinStopShare})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != CaseCheckinShare || c.Facts["checkin_stop"] != CheckinStopShare {
		t.Fatalf("case = %s, facts %v; want checkin_share with checkin_stop %q", c.Kind, c.Facts, CheckinStopShare)
	}
}

// alice and bob both close t1's live case at once. alice's answer holds
// the case from its read to its end: her close of #12 runs and her Post
// waits while bob's answer comes. bob's waits for the case, then finds it
// resolved by alice with close and is refused, with no close and no Post
// of his run: one close of #12, one Post.
func TestActHoldsTheCaseFromItsReadToItsEnd(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	closing := Resolution{Action: ActClose}
	var mu sync.Mutex
	var posts []string
	inside, release, bobPosted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	post := func(by string, wait bool) func(context.Context) (int64, error) {
		return func(context.Context) (int64, error) {
			mu.Lock()
			posts = append(posts, by)
			mu.Unlock()
			if wait {
				close(inside)
				<-release
				return 601, nil
			}
			close(bobPosted)
			return 602, nil
		}
	}
	errs := make(chan error, 2)
	go func() {
		_, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "alice", Rung: 3, Res: closing, Post: post("alice", true)})
		errs <- err
	}()
	<-inside
	var bob error
	bobDone := make(chan struct{})
	go func() {
		_, bob = g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "bob", Rung: 3, Res: closing, Post: post("bob", false)})
		close(bobDone)
	}()
	select {
	case <-bobPosted:
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-errs; err != nil {
		t.Fatalf("alice's close: %v", err)
	}
	<-bobDone
	var ended ErrCaseEnded
	if !errors.As(bob, &ended) || ended.Case.Resolution == nil || ended.Case.Resolution.By != "alice" || ended.Case.Resolution.Action != ActClose {
		t.Fatalf("bob's close = %v; want refused, the case resolved by alice with close", bob)
	}
	g.forge.mu.Lock()
	closes := g.forge.closes
	g.forge.mu.Unlock()
	if closes != 1 || !slices.Equal(posts, []string{"alice"}) {
		t.Fatalf("closes of #12 %d, posts %q; want one close and alice's Post alone", closes, posts)
	}
}

// RecordGrant retried after its commit (a timeout, a crash) writes nothing
// twice: the second run of the same identity drops its row on the events'
// identity, adds nothing to the card's row, and publishes nothing, so the
// month's pool reads the one session.
func TestRecordGrantIsOnceByItsIdentity(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	bus := NewEventBus()
	g.s.SetBus(bus)
	ch := make(chan Event, 4)
	defer bus.Subscribe("demo-app/demo-app", ch)()
	key := identityKey(EventCardExtended, "run-1", "3")
	for range 2 {
		if err := g.r.RecordGrant(ctx, "t1", "sebastian", policy.Draw{Sessions: 1}, key); err != nil {
			t.Fatal(err)
		}
	}
	if rows := g.rows(t, EventCardExtended); len(rows) != 1 || rows[0].Actor != "sebastian" || rows[0].Detail["sessions"] != 1.0 {
		t.Fatalf("card_extended rows = %+v, want sebastian's one", rows)
	}
	if drawn, err := g.s.PoolDrawn(ctx, "demo-app/demo-app", time.Now()); err != nil || drawn != (policy.Draw{Sessions: 1}) {
		t.Fatalf("the pool drew %+v, %v; want 1 session", drawn, err)
	}
	if card := g.card(t); card.Usage.RaisedSessions != 0 {
		t.Fatalf("raised sessions = %d, want 0: the run granted it", card.Usage.RaisedSessions)
	}
	if got := len(ch); got != 1 {
		t.Fatalf("published %d rows, want the one", got)
	}
}

// An answer on a trip's fresh case before the trip writes its step: the
// case's trip fact marks it a trip's under the gate all the same, so the
// human's extend of one session there raises the card's row, which the run
// reads.
func TestAnExtendOnAFreshTripCaseRaisesTheRow(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "h2",
		Problem: "Card #12 used its sessions", Facts: map[string]any{"trip": "run-1/7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: oneSession}); err != nil {
		t.Fatal(err)
	}
	if card := g.card(t); card.Usage.RaisedSessions != 1 {
		t.Fatalf("raised sessions = %d, want 1", card.Usage.RaisedSessions)
	}
}

// Two grants of one run at one attempt value, the session cap's and then
// the agent time's after a session that took no number, are two rows of
// the pool; a retry of the second writes nothing more.
func TestRecordGrantKeysEachGrant(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	first := TaskUsage{Attempts: 3, ExtraAttempts: 1}
	second := TaskUsage{Attempts: 3, ExtraAttempts: 1, ExtraTime: 5 * time.Minute}
	for _, grant := range []struct {
		d policy.Draw
		u TaskUsage
	}{{policy.Draw{Sessions: 1}, first}, {policy.Draw{AgentTime: 5 * time.Minute}, second}, {policy.Draw{AgentTime: 5 * time.Minute}, second}} {
		if err := g.r.RecordGrant(ctx, "t1", "sebastian", grant.d, grantKey("run-1", grant.u)); err != nil {
			t.Fatal(err)
		}
	}
	if rows := g.rows(t, EventCardExtended); len(rows) != 2 {
		t.Fatalf("card_extended rows = %+v, want the two grants'", rows)
	}
	if drawn, err := g.s.PoolDrawn(ctx, "demo-app/demo-app", time.Now()); err != nil || drawn != (policy.Draw{Sessions: 1, AgentTime: 5 * time.Minute}) {
		t.Fatalf("the pool drew %+v, %v; want 1 session and 5 minutes", drawn, err)
	}
}

// The judgment budget's money card is a human card to Act: its close is a
// choice, recorded and resolved with nothing run.
func TestActTakesTheJudgmentBudgetCardsClose(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app",
		IssueNumber: 13}); err != nil {
		t.Fatal(err)
	}
	q := chatBudgetQuestion(Task{ID: "t-chat-demo-app-2026-11", BudgetEUR: 10})
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "h1", Kind: IntakeEscalation, Author: "factoryd", Body: q.Ask, Question: &q, Choices: q.Labels()}); err != nil {
		t.Fatal(err)
	}
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "h1", Repository: "demo-app/demo-app", Kind: CaseJudgmentBudget, StateKey: "budget", Status: CaseAtHuman, Problem: q.Ask})
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerClose, Note: "Decided, close"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution.Action != AnswerClose || len(out.Did) != 0 {
		t.Fatalf("case = %s, %+v, did %+v; want resolved with close and nothing run", got.Status, got.Resolution, out.Did)
	}
}

func (w *caseSignals) Terminate(context.Context, string, string, string) error { return nil }

func (w *caseSignals) Alive(context.Context, string) (bool, error)       { return true, nil }
func (w *caseSignals) RunningRuns(context.Context) ([]RunningRun, error) { return nil, nil }

// The close a salvage makes (Finish, as salvage calls it) leaves the card's
// other open case to the heal, which withdraws it with the end as its reason.
func TestSalvageCloseWithdrawsTheCardsOtherCases(t *testing.T) {
	ctx := context.Background()
	g := newActRig(t)
	flag, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseMonitorFlag, StateKey: "wait_over_bound|run_quiet",
		Status: CaseAtHuman, Problem: "Monitor flag: wait_over_bound"})
	if err != nil {
		t.Fatal(err)
	}
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	g.r.completion = newCompletion(g.s, fjURL, llURL)
	g.r.kube = newFakeKube()
	if err := g.r.Finish(ctx, "t1", "closed", "salvaged into #9"); err != nil {
		t.Fatal(err)
	}
	if got, err := g.s.ReconcileCases(ctx, "demo-app/demo-app"); err != nil || got.Withdrawn != 1 {
		t.Fatalf("ReconcileCases = %+v, %v; want the one other case withdrawn", got, err)
	}
	other := caseByID(t, g.s, g.c.ID)
	if other.Status != CaseWithdrawn || other.Resolution == nil || other.Resolution.Reason != "card ended closed" {
		t.Fatalf("other case %s %+v, want withdrawn with the end as reason", other.Status, other.Resolution)
	}
	if own := caseByID(t, g.s, flag.ID); own.Status != CaseAtHuman {
		t.Fatalf("the salvage's own flag case is %s, want it left to Act's resolution", own.Status)
	}
}
