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
// case was asked by comment 70. Forgejo takes comments on issue 12 (ids
// 501 on) and closes it, and the workflows record every signal.
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

// actForge is a Forgejo for issue 12 of demo-app/demo-app: it keeps the
// comments posted and whether the issue was closed.
type actForge struct {
	mu       sync.Mutex
	comments []string
	closed   bool
}

func (f *actForge) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const issue = "/api/v1/repos/demo-app/demo-app/issues/12"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == issue+"/comments":
			var c struct{ Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &c)
			f.comments = append(f.comments, c.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, 500+len(f.comments))
		case r.Method == http.MethodGet && r.URL.Path == issue+"/dependencies":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch && r.URL.Path == issue:
			f.closed = true
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
		Status: CaseAtHuman, Problem: "Card #12 used its sessions", CommentID: 70})
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

// The human's extend of one session on the live case resolves it and
// raises the card's sessions in one transaction: the case reads resolved
// by sebastian on rung 3 with extend, its step says what was drawn, and
// the card_extended row is a pool draw of rung 3 naming the case. A second
// answer finds the case ended, names the first, and draws nothing.
func TestActResolvesAndDrawsInOneTransaction(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Comment: 77, Res: oneSession})
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
		len(last.Did) != 1 || last.Did[0].Action != ActExtend || last.Did[0].Result != "1 session from the pool" {
		t.Fatalf("the step = %+v, want sebastian's extend on rung 3, resolved, that drew 1 session from the pool", last)
	}

	_, err = g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "ana", Rung: 3, Res: oneSession})
	var ended ErrCaseEnded
	at := c.ResolvedAt.UTC().Format("15:04")
	if !errors.As(err, &ended) || err.Error() != fmt.Sprintf("case %d was resolved by sebastian with extend at %s UTC", g.c.ID, at) {
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

// With no comment of its caller, Act posts factoryd's on the issue the case
// asked on, naming the human and the action. case_resolved carries the
// case, the rung, the answer and that comment, and answer_whole the
// comment. The answer row is a session_granted: its draw holds a session.
// A caller's own comment is not posted again and rides in both signals.
func TestActSignalsTheRunAndPostsTheComment(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
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
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: second.ID, By: "sebastian", Rung: 3, Comment: 88, Res: cont}); err != nil {
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
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Comment: 77, Res: Resolution{
		Action: ActFund, Args: map[string]any{"eur": 3.0}}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "3.00 EUR more takes the budget of 3.00 EUR past the card's ceiling of 5.00 EUR: raise the ceiling first") {
		t.Fatalf("a fund of 3 = %+v, %v; want the ceiling's refusal", got, err)
	}
	res.Fund.EUR = 4
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Comment: 77, Res: res}); err != nil {
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

// The webhook's copy of the human's answer comment may resolve the case
// before Act does: Act's answer on that very comment resolves it again
// with its action and draw. Another comment's answer does not.
func TestActTakesOverTheWebhooksCopyOfItsComment(t *testing.T) {
	g := newActRig(t)
	ctx := context.Background()
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventQuestionAnswered,
		Actor: "sebastian", Via: ViaWebhook, Detail: map[string]any{"comment": int64(77)}}); err != nil {
		t.Fatal(err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseResolved || c.Resolution.Action != "answer" {
		t.Fatalf("case after the webhook's copy = %s, %+v; want resolved with answer", c.Status, c.Resolution)
	}
	_, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Comment: 78, Res: oneSession})
	var ended ErrCaseEnded
	if !errors.As(err, &ended) {
		t.Fatalf("an answer on comment 78 = %v, want the case ended", err)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: g.c.ID, By: "sebastian", Rung: 3, Comment: 77, Res: oneSession}); err != nil {
		t.Fatalf("the answer on comment 77: %v", err)
	}
	if c := caseByID(t, g.s, g.c.ID); c.Status != CaseResolved || c.Resolution.Action != ActExtend || g.card(t).Usage.RaisedSessions != 1 {
		t.Fatalf("case = %s, %+v, raised %d; want resolved with extend and 1 session raised", c.Status, c.Resolution, g.card(t).Usage.RaisedSessions)
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
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "h1", Kind: IntakeEscalation, Author: "factoryd", Body: q.Ask, Question: &q, Choices: q.Labels()}); err != nil {
		t.Fatal(err)
	}
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "h1", Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: "roll", Status: CaseAtHuman, Problem: q.Ask})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: "deploy"}})
	var refused ErrActRefused
	if want := "card #13 takes one of its choices or a note: dismiss, check_again"; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("a value it does not offer = %v, want %q", err, want)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: c.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerDismiss}}); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution.Action != AnswerDismiss {
		t.Fatalf("case = %s, %+v; want resolved with %s", got.Status, got.Resolution, AnswerDismiss)
	}
	events, err := g.s.TaskEvents(ctx, "h1", 10)
	if err != nil || len(events) != 1 || events[0].Kind != EventQuestionAnswered || events[0].Detail["value"] != AnswerDismiss {
		t.Fatalf("h1's rows = %+v, %v; want the answer row with the value", events, err)
	}
	if len(g.signals.got) != 0 || len(g.forge.comments) != 0 {
		t.Fatalf("signals %+v, comments %q; want none for a human card", g.signals.got, g.forge.comments)
	}
}
