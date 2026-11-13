package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

const rungFindingText = "TasksByPR is undefined"

// rungRig is tripRig's card t1 with a check-in model: a medium card of the
// coder class, whose head h2 on pull request 9 is green and not approved,
// and the review's one finding, rungFindingText, on h2. The check-in's
// model is a script that notes the task each call booked on.
type rungRig struct {
	tripRig
	chat *idChat
	id   string // the finding's id
}

type idChat struct {
	scriptedChat
	mu  sync.Mutex
	ids []string
}

func (c *idChat) Call(ctx context.Context, taskID, alias string, in CheckinRequest) (CheckinResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, taskID)
	return c.scriptedChat.Call(ctx, taskID, alias, in)
}

func newRungRig(t *testing.T, checks string, answers ...CheckinResponse) rungRig {
	t.Helper()
	ctx := context.Background()
	g := rungRig{tripRig: newTripRig(t, checks), chat: &idChat{scriptedChat: scriptedChat{answers: answers}}, id: findingID(rungFindingText)}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET required_capabilities = '["coder"]'::jsonb, size = 'medium', budget_eur = 5 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.atHead(t, false)
	if err := g.r.timeline.Append(ctx, "t1", "model_review", map[string]any{"commit": "h2", "verdict": "concerns", "findings": []string{rungFindingText}}); err != nil {
		t.Fatal(err)
	}
	g.r.checkin = g.chat
	g.r.cfg.Prices = aliasPrices{"big": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}}
	return g
}

// answer is the rung's answer call on rungFindingText, a dropped verdict
// with its evidence, over's keys over the defaults.
func (g rungRig) answer(t *testing.T, over map[string]any) CheckinResponse {
	t.Helper()
	a := map[string]any{"recommendation": map[string]any{"action": "successor", "grounds": "the finding is dropped"},
		"verdicts": map[string]any{g.id: map[string]any{"verdict": "dropped", "evidence": "tasks.go:12 at head: TasksByPR is declared"}}}
	for k, v := range over {
		a[k] = v
	}
	return answerCall(t, a)
}

// handUp is the answer that hands the case up recommending close.
func (g rungRig) handUp(t *testing.T) CheckinResponse {
	return g.answer(t, map[string]any{"decision": "hand_up", "draw": nil,
		"recommendation": map[string]any{"action": "close", "grounds": "the card's work is done on main"}})
}

// projectStore answers the project row of demo-app, which no policy stored
// in the rig's database names.
type projectStore struct{ *PGStore }

func (projectStore) ProjectByRepository(context.Context, string) (Project, bool, error) {
	return Project{Name: "demo-app", Repository: "demo-app/demo-app", File: true}, true, nil
}

// holdFirstCall holds the chat's first call until release closes and closes
// entered when it starts. A later call is another case's: a sweep may
// open a monitor flag's case, whose rung 1 asks the model too. It passes.
func (g rungRig) holdFirstCall() (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.chat.onCall = func() {
		once.Do(func() { close(entered); <-release })
	}
	return entered, release
}

// settle waits for the rung 1 goroutines the trips started.
func (g rungRig) settle() { g.r.rungs.Wait() }

func (g rungRig) rows(t *testing.T, kind string) []Event { return actRig{s: g.s}.rows(t, kind) }

func (g rungRig) steps(t *testing.T, id int64) []CaseStep { return caseStepsOf(t, g.s, id) }

func (g rungRig) raisedSessions(t *testing.T) int {
	t.Helper()
	var n int
	if err := g.s.db.QueryRowContext(context.Background(), `SELECT raised_sessions FROM tasks WHERE id = 't1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (g rungRig) signalsOf(name string) []reopenCall {
	g.signals.mu.Lock()
	defer g.signals.mu.Unlock()
	var out []reopenCall
	for _, s := range g.signals.got {
		if s.signal == name {
			out = append(out, s)
		}
	}
	return out
}

// drawn books a draw of d on rung for t1 as the rungs' draws are booked.
func (g rungRig) drawn(t *testing.T, rung int, d policy.Draw, key string) {
	t.Helper()
	v := g.r.cfg.Policy()
	_, project, _ := v.ProjectOf("demo-app/demo-app")
	esc := v.EscalationOf("demo-app/demo-app")
	if _, err := g.s.DrawCard(context.Background(), "t1", CaseDraw{Rung: rung, Extend: d, PolicyCeiling: CardCeiling(v.Document, project),
		CardBound: esc.CardDrawOf("medium"), Pool: esc.Pool, RunGrants: true,
		Event: Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"pool": true, "rung": rung}, IdentityKey: key}}); err != nil {
		t.Fatal(err)
	}
}

// A medium card at its session cap with a review finding: rung 0 hands it
// up, rung 1 reads one tool and answers extend with one session and a
// dropped verdict. By hand: the pool holds 30 sessions and the medium
// card's draw 2, so one session fits; the extend adds 1 session to the
// card (raised_sessions 1) and books a pool row of rung 1 (card_extended
// with pool true, rung 1); the answer resolves the case on rung 1, so the
// step is rung 1's own, ended with what it read, found, did, decided and
// cost (0.02 for the evidence call and 0.01 for the answer), and no human
// is asked: no question row, one case_resolved signal.
func TestRung1ResolvesWithinBound(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{{ToolName: "card_facts", UsageEUR: 0.02}, g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	g.chat.answers[1].UsageEUR = 0.01
	out := g.trip(t, tripCap)
	if out.Rung != 1 || out.Resolution != nil || out.Comment != 0 || out.CaseID == 0 {
		t.Fatalf("outcome = %+v, want the case held by rung 1 with no comment and no resolution", out)
	}
	g.settle()

	c := caseByID(t, g.s, out.CaseID)
	if c.Status != CaseResolved || c.Resolution == nil || c.Resolution.By != CheckinDecider || c.Resolution.Rung != 1 || c.Resolution.Action != ActExtend {
		t.Fatalf("case = %+v, resolution %+v; want resolved by the check-in on rung 1 with extend", c, c.Resolution)
	}
	steps := g.steps(t, c.ID)
	if len(steps) != 2 || steps[0].Rung != 0 || steps[1].Rung != 1 {
		t.Fatalf("steps = %+v, want rung 0's hand-up then rung 1's one step", steps)
	}
	s := steps[1]
	wantDid := []CaseAction{
		{Action: "verdict", Args: map[string]any{"id": g.id, "verdict": "dropped", "evidence": "tasks.go:12 at head: TasksByPR is declared"}},
		{Action: ActExtend, Args: map[string]any{"sessions": 1.0, "agent_time_s": 0.0}, Result: "Added 1 session from the pool."},
	}
	wantFound := "the stop reads as gate_stop; finding " + g.id + " dropped: tasks.go:12 at head: TasksByPR is declared"
	if s.Actor != CheckinDecider || s.Outcome != StepResolved || s.Decision != ActExtend || s.Grounds != "one more session" || s.Found != wantFound ||
		!reflect.DeepEqual(s.Did, wantDid) || len(s.Read) != 1 || s.Read[0].Tool != "card_facts" || s.CostEUR != 0.03 || s.EndedAt == nil {
		t.Fatalf("rung 1's step = %+v\nwant found %q, did %+v, one read of card_facts, cost 0.03", s, wantFound, wantDid)
	}
	if got := g.raisedSessions(t); got != 1 {
		t.Fatalf("raised_sessions = %d, want 1", got)
	}
	rows := g.rows(t, EventCardExtended)
	if len(rows) != 1 || rows[0].Detail["pool"] != true || rows[0].Detail["rung"] != 1.0 {
		t.Fatalf("card_extended rows = %+v, want one pool row of rung 1", rows)
	}
	if asks := g.asks(t); len(asks) != 0 {
		t.Fatalf("asks = %+v, want none", asks)
	}
	if sig := g.signalsOf(SignalCaseResolved); len(sig) != 1 || sig[0].sig.Case.CaseID != c.ID || sig[0].sig.Case.Res.Action != ActExtend || sig[0].sig.Case.Rung != 1 {
		t.Fatalf("case_resolved signals = %+v, want one for case %d with extend on rung 1", sig, c.ID)
	}
	if want := ChatTaskID("demo-app", time.Now()); !slices.Equal(g.chat.ids, []string{want, want}) || !slices.Equal(g.chat.aliases, []string{"big", "big"}) {
		t.Fatalf("calls booked on %v under %v, want the project's chat task %s on the check-in alias big", g.chat.ids, g.chat.aliases, want)
	}
}

// The same card with two sessions drawn already, one on rung 0 and one on
// rung 1: the medium card's draw bound of 2 sessions, 1h and 4 EUR leaves 0
// sessions, so the extend is refused naming it, rung 1's step ends handed
// up with the refusal in its found and the answer's recommendation as its
// decision, and the case goes to the human.
func TestRung1RefusedDrawGoesToTheHuman(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1},
		"recommendation": map[string]any{"action": "successor", "grounds": "one more session would do"}})}
	// the two draws land while the rung reads, after its statement offered
	// the extend: Act reads the card's draws again
	g.chat.onCall = func() {
		g.drawn(t, 0, policy.Draw{Sessions: 1}, "k0")
		g.drawn(t, 1, policy.Draw{Sessions: 1}, "k1")
	}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)

	refusal := fmt.Sprintf("the card's draw bound leaves 0 sessions, %s of agent time and 4.00 EUR: rung 2 or the human may draw more", minutesWord(policy.SizeAgentTime("medium", time.Hour)))
	steps := g.steps(t, out.CaseID)
	if len(steps) != 4 || steps[1].Rung != 1 || steps[1].Outcome != StepHandedUp || !strings.Contains(steps[1].Found, "rung 1's answer extend was refused: "+refusal) ||
		steps[1].Decision != "successor" || steps[1].Grounds != "one more session would do" || steps[2].Rung != 2 || steps[3].Rung != 3 {
		t.Fatalf("steps = %+v, want rung 1's step handed up naming %q, then rung 2's expiry and the human's", steps, refusal)
	}
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman {
		t.Fatalf("case status = %s, want at_human", c.Status)
	}
	if got := g.raisedSessions(t); got != 0 {
		t.Fatalf("raised_sessions = %d, want 0: the refused draw drew nothing", got)
	}
	if len(g.asks(t)) != 1 || len(g.forge.comments) != 1 {
		t.Fatalf("asks %d, comments %d, want one face", len(g.asks(t)), len(g.forge.comments))
	}
}

// A hand_up recommending close: the step ends handed up with the
// recommendation as its decision and the grounds, the case waits at the
// human, and the one face is one issue comment and one question_asked row
// naming the case, whose recommendation holds the grounds.
func TestRung1HandsUpRecommendation(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)

	steps := g.steps(t, out.CaseID)
	if len(steps) != 4 || steps[1].Rung != 1 || steps[1].Outcome != StepHandedUp || steps[1].Decision != "close" || steps[1].Grounds != "the card's work is done on main" {
		t.Fatalf("steps = %+v, want rung 1's step handed up recommending close with its grounds", steps)
	}
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman || c.OwnerRung != 3 || c.NextWakeAt != nil {
		t.Fatalf("case = %+v, want at the human, owned by the human with no wake", c)
	}
	asks := g.asks(t)
	if len(g.forge.comments) != 1 || len(asks) != 1 || asks[0].Detail["case_id"] != float64(out.CaseID) ||
		!strings.Contains(fmt.Sprint(asks[0].Detail["recommendation"]), "the card's work is done on main") ||
		!strings.Contains(fmt.Sprint(asks[0].Detail["recommendation"]), "close") {
		t.Fatalf("comments %q, asks %+v; want one comment and one ask of the case recommending close with the grounds", g.forge.comments, asks)
	}
	if len(g.signalsOf(SignalCaseResolved)) != 0 {
		t.Fatal("a hand-up resolved nothing, and the run was signalled")
	}
	if faced := g.signalsOf(SignalCaseFaced); len(faced) != 1 || faced[0].sig.Case.CaseID != out.CaseID || faced[0].sig.Case.Comment != caseByID(t, g.s, out.CaseID).CommentID || faced[0].sig.Case.Comment == 0 {
		t.Fatalf("case_faced signals = %+v, want one that tells the run the face's comment, as the trip returned none", faced)
	}
}

// The answer's tool is forced: a decision outside the enum is refused with
// the refusal named and asked again once, and the valid answer after it
// resolves; two bad answers hand the case up, and Act is never called: no
// answer row, no draw row.
func TestRung1ForcedToolCall(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "teleport"}), g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	out := g.trip(t, tripCap)
	g.settle()
	if g.chat.calls != 2 || !strings.Contains(g.chat.gots[1].Input, `Your last answer was unreadable: `) || !strings.Contains(g.chat.gots[1].Input, "teleport") {
		t.Fatalf("%d calls, the second asked %q; want one repair that names the refusal", g.chat.calls, g.chat.gots[1].Input)
	}
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseResolved || c.Resolution.Action != ActExtend {
		t.Fatalf("case = %+v, want it resolved by the repaired answer", c)
	}
	for _, call := range g.chat.gots {
		if call.ToolChoice != nil && !reflect.DeepEqual(call.ToolChoice, forcedToolChoice(checkinAnswerTool)) {
			t.Fatalf("tool choice %v, want the answer tool", call.ToolChoice)
		}
	}

	bad := newRungRig(t, "success")
	bad.chat.answers = []CheckinResponse{bad.answer(t, map[string]any{"decision": "teleport"}), bad.answer(t, map[string]any{"decision": "teleport"})}
	out = bad.trip(t, tripCap)
	bad.settle()
	bad.afterRung2(t, out.CaseID)
	steps := bad.steps(t, out.CaseID)
	if len(steps) != 4 || steps[1].Outcome != StepHandedUp || !strings.Contains(steps[1].Found, "the rung's answers were refused twice") {
		t.Fatalf("steps = %+v, want rung 1's step handed up on two refused answers", steps)
	}
	if len(bad.rows(t, EventQuestionAnswered)) != 0 || len(bad.rows(t, EventCardExtended)) != 0 || len(bad.signalsOf(SignalCaseResolved)) != 0 || bad.raisedSessions(t) != 0 {
		t.Fatal("Act ran on an unreadable answer: want no answer row, no draw row and no signal")
	}
	if c := caseByID(t, bad.s, out.CaseID); c.Status != CaseAtHuman {
		t.Fatalf("case status = %s, want at_human", c.Status)
	}
}

// A trip that finds its case live with rung 1 at work appends rung 0's
// step "the same stop again" and returns the case with rung 1, starting no
// second conversation: the script is called once.
func TestRung1RepeatTripStartsNoSecondConversation(t *testing.T) {
	g := newRungRig(t, "success")
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	first := g.trip(t, tripCap)
	again := g.trip(t, tripCap)
	retry, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: tripCap}, tripAttempt{ID: "run/1", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	g.settle()
	if again.CaseID != first.CaseID || again.Rung != 1 || again.Comment != 0 || retry.CaseID != first.CaseID || retry.Rung != 1 {
		t.Fatalf("repeat %+v, retry %+v; want case %d held by rung 1 each", again, retry, first.CaseID)
	}
	steps := g.steps(t, first.CaseID)
	if g.chat.calls != 1 || len(steps) < 3 || steps[1].Rung != 1 {
		t.Fatalf("%d conversations, steps %+v; want one conversation", g.chat.calls, steps)
	}
	repeat := 0
	for _, s := range steps {
		if s.Rung == 0 && strings.HasPrefix(s.Found, "the same stop again") {
			repeat++
		}
	}
	if repeat != 1 {
		t.Fatalf("steps = %+v, want rung 0's one step of the same stop again", steps)
	}
}

// A rung 1 step past its due climbs once: the sweep ends it handed up as
// unanswered within 35m and the human is asked, one face; the rung's late
// answer is refused as too late and asks nobody.
func TestRung1ExpiryClimbsOnce(t *testing.T) {
	g := newRungRig(t, "success")
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	out := g.trip(t, tripCap)
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 1`, out.CaseID); err != nil {
		t.Fatal(err)
	}
	steps := g.steps(t, out.CaseID)
	g.r.now = func() time.Time { return steps[1].DueAt.Add(time.Minute) }
	for range 2 {
		expired, err := g.s.ExpireSteps(ctx, g.r.clock())
		if err != nil {
			t.Fatal(err)
		}
		if err := g.r.climbExpired(ctx, expired); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	g.settle()
	steps = g.steps(t, out.CaseID)
	if len(steps) != 3 || steps[1].Outcome != StepHandedUp || !strings.HasSuffix(steps[1].Found, "the rung did not answer within 35m") || steps[2].Rung != 2 || len(g.forge.comments) != 0 {
		t.Fatalf("steps = %+v, %d comments, want rung 1's step handed up as unanswered within 35m and rung 2 at work, no face", steps, len(g.forge.comments))
	}
	g.afterRung2(t, out.CaseID)
	steps = g.steps(t, out.CaseID)
	if len(steps) != 4 || steps[3].Rung != 3 {
		t.Fatalf("steps = %+v, want the human's step after rung 2's bound", steps)
	}
	if len(g.forge.comments) != 1 || len(g.asks(t)) != 1 || caseByID(t, g.s, out.CaseID).Status != CaseAtHuman {
		t.Fatalf("comments %d, asks %d, want the one face", len(g.forge.comments), len(g.asks(t)))
	}
	if len(g.rows(t, EventCardExtended)) != 0 || len(g.signalsOf(SignalCaseResolved)) != 0 || g.raisedSessions(t) != 0 || len(g.signalsOf(SignalCaseFaced)) != 1 {
		t.Fatal("the late extend drew, resolved the case at the human or signalled the run: want it refused as too late, and one face signal")
	}
}

// A moment's own human answer is rung 1's step: the case faces the human
// with no conversation, and the step is the moment's record. A goal
// conflict is the human's alone and goes to rung 2 for its analysis alone;
// any other reason goes to rung 2 too, since rung 1 never judges one stop
// twice.
func TestRung1MomentHumanIsItsStep(t *testing.T) {
	for _, kind := range []string{CheckGoalConflict, CheckSpend} {
		g := newRungRig(t, "success")
		out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: "the check-in sends this to a human: " + kind, Kind: CaseCheckinHuman,
			StateKey: "k-" + kind, Extra: map[string]any{"reason_kind": kind}}, tripAttempt{N: 1})
		if err != nil {
			t.Fatal(err)
		}
		g.settle()
		// every reason goes to rung 2 first, a human-only one for its analysis; its bound ends, then the face
		if steps := g.steps(t, out.CaseID); out.Rung != 2 || g.chat.calls != 0 || len(steps) != 3 || steps[1].Decision != RungHandUp || !strings.Contains(steps[1].Grounds, kind) || steps[2].Rung != 2 || steps[2].Outcome != StepInProgress {
			t.Fatalf("%s: outcome %+v, steps %+v; want rung 2 at work after the moment's record", kind, out, steps)
		}
		g.afterRung2(t, out.CaseID)
		steps := g.steps(t, out.CaseID)
		if len(steps) != 4 || steps[2].Outcome != StepHandedUp || steps[3].Rung != 3 || len(g.forge.comments) != 1 {
			t.Fatalf("%s: steps %+v, comments %d; want the face after rung 2's bound", kind, steps, len(g.forge.comments))
		}
	}
}

// A human card and a merge approval reach the trip but run no rung 1: Act
// refuses rung 1 an answer on them.
func TestRung1SkipsHumanCardsAndMergeApprovals(t *testing.T) {
	for _, kind := range []CaseKind{CaseHumanCard, CaseMergeApproval} {
		g := newRungRig(t, "success")
		out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: "a stop of " + string(kind), Kind: kind, StateKey: "k"}, tripAttempt{N: 1})
		if err != nil {
			t.Fatal(err)
		}
		g.settle()
		if out.Rung != 3 || g.chat.calls != 0 || slices.ContainsFunc(g.steps(t, out.CaseID), func(s CaseStep) bool { return s.Rung == 1 }) {
			t.Fatalf("%s: outcome %+v, %d conversations; want the face at once and no step of rung 1", kind, out, g.chat.calls)
		}
	}
}

// With escalation.judgment autonomous_and_above on a manual project the
// rungs do not judge: no conversation, the face at once.
func TestRung1DoesNotJudgeAtManualWithAutonomousJudgment(t *testing.T) {
	g := newRungRig(t, "success")
	v := g.r.cfg.Policy()
	p := v.Projects["demo-app"]
	p.Autonomy = policy.AutonomyManual
	p.Escalation = &policy.Escalation{Judgment: policy.JudgmentAutonomousAndAbove}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.cfg.Policy = func() policy.Version { return v }
	out := g.trip(t, tripCap)
	g.settle()
	if out.Rung != 3 || g.chat.calls != 0 || slices.ContainsFunc(g.steps(t, out.CaseID), func(s CaseStep) bool { return s.Rung == 1 }) || len(g.forge.comments) != 1 {
		t.Fatalf("outcome %+v, %d conversations, %d comments; want the face at once and no rung 1", out, g.chat.calls, len(g.forge.comments))
	}
}

// With no check-in alias, a card of no class, the case goes from rung 0 to
// rung 2: no step of rung 1, no face until rung 2's bound ends.
func TestRung1NoAliasGoesToRung2(t *testing.T) {
	g := newRungRig(t, "success")
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, tripCap)
	g.settle()
	if out.Rung != 2 || len(g.forge.comments) != 0 || g.chat.calls != 0 || slices.ContainsFunc(g.steps(t, out.CaseID), func(s CaseStep) bool { return s.Rung == 1 }) {
		t.Fatalf("outcome %+v, %d comments; want rung 2 at work, no face and no rung 1", out, len(g.forge.comments))
	}
	g.afterRung2(t, out.CaseID)
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("case %s with %d comments; want the face after rung 2's bound", c.Status, len(g.forge.comments))
	}
}

// Rung 1's tools are the check-in's read tools without run and git_log.
// read_file reads the case's head through Forgejo, main when the case has
// none; a path outside the repository is refused.
func TestRung1ToolsAreReadOnlyAndReadTheHead(t *testing.T) {
	names := toolNames(caseEvidence())
	if !slices.Contains(names, "read_file") || !slices.Contains(names, "grep") || slices.Contains(names, "run") || slices.Contains(names, "git_log") {
		t.Fatalf("tools = %v, want read_file and grep and no run or git_log", names)
	}
	g := newRungRig(t, "success")
	g.forge.files = map[string]string{"h2:src/tasks.go": "package tasks // at the head", "main:src/tasks.go": "package tasks // at main"}
	repo := Repo{Owner: "demo-app", Name: "demo-app"}
	tools := &caseTools{r: g.r, t: Task{ID: "t1"}, f: CaseFacts{Head: "h2"}, repo: repo}
	if out, _, err := tools.Tool(context.Background(), "read_file", map[string]any{"path": "/workspace/src/tasks.go"}); err != nil || out != "package tasks // at the head" {
		t.Fatalf("read_file at the head = %q, %v", out, err)
	}
	tools.f.Head = ""
	if out, _, err := tools.Tool(context.Background(), "read_file", map[string]any{"path": "src/tasks.go"}); err != nil || out != "package tasks // at main" {
		t.Fatalf("read_file with no head = %q, %v, want main's", out, err)
	}
	if _, _, err := tools.Tool(context.Background(), "read_file", map[string]any{"path": "../../etc/passwd"}); err == nil {
		t.Fatal("a path outside the repository was read")
	}
	if _, _, err := tools.Tool(context.Background(), "run", map[string]any{"command": "ls"}); err == nil {
		t.Fatal("run was answered")
	}
}

// grep goes through the checkout of the head and main, or of main alone
// for a case with no pull request; every output is cut and scrubbed.
func TestRung1GrepSearchesTheHeadAndMain(t *testing.T) {
	dir := gitFixture(t)
	old := checkoutRefs
	var gotPR int64
	checkoutRefs = func(_ context.Context, _ *Forgejo, _ Repo, pr int64, _ string) (string, []namedRef, func(), error) {
		gotPR = pr
		if pr == 0 {
			return dir, []namedRef{{"main", "main"}}, func() {}, nil
		}
		return dir, []namedRef{{"head", "head"}, {"main", "main"}}, func() {}, nil
	}
	t.Cleanup(func() { checkoutRefs = old })
	g := newRungRig(t, "success")
	tools := &caseTools{r: g.r, t: Task{ID: "t1"}, f: CaseFacts{Head: "c1", PR: 9}, repo: Repo{Owner: "demo-app", Name: "demo-app"}}
	defer tools.close()
	out, _, err := tools.Tool(context.Background(), "grep", map[string]any{"pattern": "Sym", "ref": "main"})
	if err != nil || out != "main:a.go:3:func Sym() {}" || gotPR != 9 {
		t.Fatalf("grep main = %q, %v (pr %d), want main's one hit through the pull request's checkout", out, err, gotPR)
	}
	both, _, err := tools.Tool(context.Background(), "grep", map[string]any{"pattern": "Sym"})
	if err != nil || !strings.Contains(both, "head:b.go:2:var _ = Sym") || !strings.Contains(both, "main:a.go:3:func Sym() {}") {
		t.Fatalf("grep both = %q, %v", both, err)
	}
	if none, _, err := tools.Tool(context.Background(), "grep", map[string]any{"pattern": "Absent"}); err != nil || none != "no match" {
		t.Fatalf("grep no match = %q, %v", none, err)
	}
	bare := &caseTools{r: g.r, t: Task{ID: "t1"}, f: CaseFacts{}, repo: Repo{Owner: "demo-app", Name: "demo-app"}}
	defer bare.close()
	if out, _, err := bare.Tool(context.Background(), "grep", map[string]any{"pattern": "Sym"}); err != nil || out != "main:a.go:3:func Sym() {}" || gotPR != 0 {
		t.Fatalf("grep with no pull request = %q, %v (pr %d), want main alone", out, err, gotPR)
	}
}

// The statement names the head's commits after the session's push, each as
// "<sha> <author> <subject>", oldest first: here two the factory's bot
// made, a rerun's and a merge of main.
func TestRung1StatementListsTheHeadCommits(t *testing.T) {
	g := newRungRig(t, "success")
	if err := g.r.timeline.Append(context.Background(), "t1", "pushed", map[string]any{"session": "s1", "branch": "factory/t1", "commit": "p1"}); err != nil {
		t.Fatal(err)
	}
	g.forge.prCommits = `[
	 {"sha":"h2","commit":{"message":"Merge branch 'main' into factory/t1\n\nbody","author":{"name":"Factory"}},"author":{"login":"factory-agent"}},
	 {"sha":"c1","commit":{"message":"run the checks again","author":{"name":"Factory"}},"author":{"login":"factory-agent"}},
	 {"sha":"p1","commit":{"message":"the session's work","author":{"name":"agent"}},"author":{"login":"agent"}}]`
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	g.trip(t, tripCap)
	g.settle()
	in := g.chat.gots[0].Input
	if !strings.Contains(in, "Commits on the head after the newest session's push, oldest first:\n- c1 factory-agent run the checks again\n- h2 factory-agent Merge branch 'main' into factory/t1\n") ||
		strings.Contains(in, "the session's work") {
		t.Fatalf("statement = %q, want the two commits after the push, oldest first, with the bot's login", in)
	}
}

// A red head: the checks' failed tests come from the job's log into the
// facts and the statement names the failing test; the handover the rung
// answers carries them at its end, written by code, cut at 800 characters.
func TestRung1RedChecksReachTheStatementAndTheNote(t *testing.T) {
	g := newRungRig(t, "failure")
	g.r.store = projectStore{g.s}
	g.forge.redLog = logLines("--- FAIL: TestSlugTrims (0.00s)", "FAIL\texample.com/slug\t1s")
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET size = 'medium' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	first := g.trip(t, tripCap)
	if first.Resolution == nil || first.Resolution.Action != ActRerunChecks {
		t.Fatalf("first outcome = %+v, want rung 0's rerun", first)
	}
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "handover", "model": "same", "draw": nil, "note": "fix the slug test"})}
	out := g.trip(t, tripCap)
	g.settle()
	if out.Rung != 1 {
		t.Fatalf("second outcome = %+v, want rung 1", out)
	}
	if in := g.chat.gots[0].Input; !strings.Contains(in, "The checks on h2 are red:\n- TestSlugTrims (example.com/slug): failed") {
		t.Fatalf("statement = %q, want the failed test named", in)
	}
	c := caseByID(t, g.s, out.CaseID)
	wantTail := "The checks on h2 are red:\n- TestSlugTrims (example.com/slug): failed"
	if c.Status != CaseResolved || !strings.HasSuffix(c.Resolution.Reason, wantTail) || !strings.Contains(c.Resolution.Reason, "fix the slug test") {
		t.Fatalf("case = %+v, resolution %+v; want a handover whose note ends with %q", c, c.Resolution, wantTail)
	}
}

// The failed-test lines of a session's note are cut at 800 characters, the
// cut marked, and a green head adds none.
func TestSessionNoteCutsTheChecksLog(t *testing.T) {
	r := &Runner{Scrub: nil}
	f := CaseFacts{Head: "0123456789abcdef", Checks: "red"}
	for i := range 100 {
		f.FailedTests = append(f.FailedTests, FailedTest{Test: fmt.Sprintf("TestNumber%03d", i), Package: "example.com/long/package/path", Kind: "failed"})
	}
	log := checksLog(f, checksLogCut)
	if len(log) > checksLogCut+len("…") || !strings.HasPrefix(log, "The checks on 01234567 are red:\n- TestNumber000 (example.com/long/package/path): failed\n") || !strings.HasSuffix(log, "…") {
		t.Fatalf("log = %q (%d bytes), want the header, the first tests and a cut at 800", log, len(log))
	}
	note := r.sessionNote(f, ActInput{Rung: 3, Res: Resolution{Action: ActContinue, Note: "go on"}})
	if !strings.HasPrefix(note, "go on\n\nThe checks on 01234567 are red:") || len(note) > len("go on\n\n")+checksLogCut+len("…") {
		t.Fatalf("note = %q, want the human's note then the cut log", note)
	}
	f.Checks = "green"
	if note := r.sessionNote(f, ActInput{Rung: 3, Res: Resolution{Action: ActContinue, Note: "go on"}}); note != "go on" {
		t.Fatalf("note on green = %q, want the note alone", note)
	}
	if note := r.sessionNote(CaseFacts{Checks: "red", Head: "h", FailedTests: f.FailedTests}, ActInput{Rung: 3, Res: Resolution{Action: ActClose, Note: "bye"}}); note != "bye" {
		t.Fatalf("a close's note = %q, want it untouched: no session starts", note)
	}
}

// Findings first: a rung's answer that starts a session hands it the first
// finding as its one job, the others after it in the order the gates raised
// them, then the rung's own note; a finding the rung dropped is no job.
// This is what keeps a survey from restarting.
func TestSessionNoteLeadsWithTheFindings(t *testing.T) {
	r := &Runner{}
	f := CaseFacts{Head: "h2", Findings: []string{"i1", "i2", "i3"}, FindingOrder: []string{"i3", "i1", "i2"},
		FindingTexts: map[string]string{"i1": "first by id", "i2": "second by id", "i3": "raised first"}}
	res := Resolution{Action: ActHandover, Args: map[string]any{"model": "same"}, Note: "start there",
		Verdicts: map[string]CheckVerdict{"second by id": {State: CheckDropped}, "raised first": {State: CheckConfirmed}}}
	got := r.sessionNote(f, ActInput{Rung: 1, Res: res})
	want := "Your one job: raised first\nThen, in order:\n- first by id\n\nstart there"
	if got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
	if got := r.sessionNote(f, ActInput{Rung: 3, Res: res}); got != "start there" {
		t.Fatalf("the human's note = %q, want it untouched", got)
	}
}

// A session the sandbox killed for its memory reaches rung 1 with the kill
// in its statement, and the handover it answers carries the kill beside the
// rung's own scoped test command: the next session is told what was killed
// and what to run instead.
func TestRung1KilledSessionHandsOverTheScopedCommand(t *testing.T) {
	g := newRungRig(t, "success")
	killed := &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "agent", Command: "go test -race ./..."}
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "handover", "model": "same", "draw": nil,
		"note": "run go test -race ./internal/slug/ only"})}
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: tripCap, Head: "h2", Killed: killed}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.settle()
	if out.Rung != 1 {
		t.Fatalf("outcome = %+v, want rung 1", out)
	}
	if in := g.chat.gots[0].Input; !strings.Contains(in, "The sandbox killed the last session for its memory (OOMKilled, exit code 137) while it ran: go test -race ./...") {
		t.Fatalf("statement = %q, want the kill with the command it ran", in)
	}
	c := caseByID(t, g.s, out.CaseID)
	note := c.Resolution.Reason
	if c.Status != CaseResolved || !strings.Contains(note, "run go test -race ./internal/slug/ only") ||
		!strings.Contains(note, "The sandbox killed the last session for its memory while it ran: go test -race ./...") {
		t.Fatalf("case = %+v, note %q; want the scoped command and the kill in the handover", c, note)
	}
}

// Rung 1 answers on the first trip of a surveying card with the first
// finding as the one job: the note starts with it.
func TestRung1HandoverLeadsWithTheFindings(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "handover", "model": "same", "draw": nil, "note": "write first, survey later",
		"verdicts": map[string]any{g.id: map[string]any{"verdict": "confirmed", "evidence": "tasks.go:1 at head: no TasksByPR"}}})}
	out := g.trip(t, tripCap)
	g.settle()
	c := caseByID(t, g.s, out.CaseID)
	if want := "Your one job: " + rungFindingText + "\n\nwrite first, survey later"; c.Status != CaseResolved || c.Resolution.Reason != want {
		t.Fatalf("case %+v, note %q; want the note %q", c, c.Resolution.Reason, want)
	}
}

// Every text of rung 1 that a human or a model reads is masked: the read
// list's arguments, a hand-up's found and grounds, the face. A URL's user
// info and a bearer value never reach them.
func TestRung1MasksWhatItRecords(t *testing.T) {
	const url, bearer = "https://user:pass@host/x", "Authorization: Bearer abc123def456ghi789"
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{
		{ToolName: "read_file", ToolArgs: map[string]any{"path": url + " " + bearer}},
		g.answer(t, map[string]any{"decision": "hand_up", "draw": nil,
			"recommendation": map[string]any{"action": "close", "grounds": "see " + url + " with " + bearer}}),
	}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	steps := g.steps(t, out.CaseID)
	faces := strings.Join(g.forge.comments, "\n")
	asks := g.asks(t)
	blob, _ := json.Marshal([]any{steps, asks, faces})
	for _, secret := range []string{"user:pass", "abc123def456ghi789"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("%q reached the record: %s", secret, blob)
		}
	}
	if len(steps) != 4 || len(steps[1].Read) != 1 || steps[1].Read[0].Tool != "read_file" {
		t.Fatalf("steps = %+v, want one read of read_file", steps)
	}
	// an end that could not carry the answer out is masked as well
	g2 := newRungRig(t, "success")
	g2.chat.onCall = func() {}
	g2.chat.answers = []CheckinResponse{g2.handUp(t)}
	out2 := g2.trip(t, tripCap)
	g2.settle()
	c := caseByID(t, g2.s, out2.CaseID)
	step, err := g2.s.AppendCaseStep(context.Background(), CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider})
	if err != nil {
		t.Fatal(err)
	}
	boom := fmt.Errorf("could not write: POST %s: %s", url, bearer)
	g2.r.rungUp(context.Background(), Task{ID: "t1", Repository: "demo-app/demo-app", IssueNumber: 12}, c, CaseFacts{}, step, RungOutcome{}, "", "", "rung 1 could not run: "+boom.Error(), nil)
	var got string
	for _, s := range g2.steps(t, c.ID) {
		if s.ID == step {
			got = s.Found
		}
	}
	if strings.Contains(got, "user:pass") || strings.Contains(got, "abc123def456ghi789") || !strings.Contains(got, "could not run") {
		t.Fatalf("found = %q, want it masked", got)
	}
}

// Rung 1's verdicts reach the run: the resolution of a taken answer
// carries them keyed by the finding's text.
func TestRung1ResolutionCarriesTheVerdictsByText(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	g.trip(t, tripCap)
	g.settle()
	sig := g.signalsOf(SignalCaseResolved)
	want := map[string]CheckVerdict{rungFindingText: {State: CheckDropped, Evidence: "tasks.go:12 at head: TasksByPR is declared"}}
	if len(sig) != 1 || !reflect.DeepEqual(sig[0].sig.Case.Res.Verdicts, want) {
		t.Fatalf("signals = %+v, want the verdicts %+v keyed by the finding's text", sig, want)
	}
}

// A check-in whose every call was refused raises checkin_refused once no
// check-in ran after it, keyed by the check-in's place in the card's life.
func TestDigestFlagsARefusedCheckin(t *testing.T) {
	g := newDigestRig(nil)
	g.line(t, 12, "checkin", map[string]any{"moment": "gate", "decision": "handover", "reason": checkinRefusedReason + "provider refused"})
	d, err := g.r.CardDigest(context.Background(), "t1410")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Flags, func(f DigestFlag) bool { return f.Name == "checkin_refused" }) {
		t.Fatalf("flags = %+v, want checkin_refused", d.Flags)
	}
	g.line(t, 13, "checkin", map[string]any{"moment": "gate", "decision": "handover", "reason": "the findings stand", "handover": "do x"})
	d, err = g.r.CardDigest(context.Background(), "t1410")
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(d.Flags, func(f DigestFlag) bool { return f.Name == "checkin_refused" }) {
		t.Fatalf("flags = %+v, want none once a check-in ran after the refused one", d.Flags)
	}
}

// A secret a tool reads reaches the model masked: the file the rung reads
// holds a URL's user info and a bearer value, and the next call's statement
// holds neither.
func TestRung1ToolOutputIsMaskedForTheModel(t *testing.T) {
	g := newRungRig(t, "success")
	g.forge.files = map[string]string{"h2:cfg.go": `const url = "https://user:pass@host/x" // Authorization: Bearer abc123def456ghi789`}
	g.chat.answers = []CheckinResponse{{ToolName: "read_file", ToolArgs: map[string]any{"path": "cfg.go"}}, g.handUp(t)}
	g.trip(t, tripCap)
	g.settle()
	if len(g.chat.gots) != 2 {
		t.Fatalf("%d calls, want 2", len(g.chat.gots))
	}
	in := g.chat.gots[1].Input
	if !strings.Contains(in, "const url") || strings.Contains(in, "user:pass") || strings.Contains(in, "abc123def456ghi789") {
		t.Fatalf("the file as the model reads it = %q, want it read and masked", in)
	}
}

// A human's answer after a face is recorded under the human: the face's
// step is the human's own to end, with the human as its actor.
func TestAHumansAnswerAfterAFaceIsRecordedUnderTheHuman(t *testing.T) {
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: oneSession}); err != nil {
		t.Fatal(err)
	}
	steps := g.steps(t, out.CaseID)
	last := steps[len(steps)-1]
	if last.Rung != 3 || last.Actor != "sebastian" || last.Outcome != StepResolved || last.Decision != ActExtend {
		t.Fatalf("steps = %+v, want the one step of rung 3 ended by sebastian with extend", steps)
	}
	if n := len(slices.DeleteFunc(slices.Clone(steps), func(s CaseStep) bool { return s.Rung != 3 })); n != 1 {
		t.Fatalf("%d steps of rung 3, want one", n)
	}
}

// A capped or failed check-in is a case rung 1 judges: its handover leads
// with the finding as the next session's one job, the rung's own note after
// it, so the session does not start the survey again.
func TestRung1HandoverAfterACappedOrFailedCheckinLeadsWithTheFinding(t *testing.T) {
	for _, tc := range []struct {
		kind CaseKind
		stop string
	}{{CaseCheckinShare, CheckinStopCallCap}, {CaseCheckinFailed, ""}} {
		g := newRungRig(t, "success")
		g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "handover", "model": "same", "draw": nil, "note": "write first, survey later",
			"verdicts": map[string]any{g.id: map[string]any{"verdict": "confirmed", "evidence": "tasks.go:1 at head: no TasksByPR"}}})}
		out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: "the check-in did not finish", Kind: tc.kind, StateKey: "k", CheckinStop: tc.stop, Head: "h2"}, tripAttempt{N: 1})
		if err != nil {
			t.Fatal(err)
		}
		g.settle()
		c := caseByID(t, g.s, out.CaseID)
		want := "Your one job: " + rungFindingText + "\n\nwrite first, survey later"
		if c.Status != CaseResolved || c.Resolution.Action != ActHandover || c.Resolution.Reason != want {
			t.Fatalf("%s: case %+v, resolution %+v; want the handover %q", tc.kind, c, c.Resolution, want)
		}
	}
}

type failingResolve struct{ *PGStore }

func (failingResolve) ResolveCase(context.Context, int64, CaseResolution, *CaseDraw, *CaseStep, *TaskSignal) error {
	return fmt.Errorf("write failed: POST https://user:pass@host/x: Authorization: Bearer abc123def456ghi789")
}

// An answer Act could not carry out, whose error carries a URL's user info
// and a bearer value, reaches neither the step's found, the face's comment
// nor the question asked unmasked, and the case is faced once.
func TestRung1MasksAnActFailureEndToEnd(t *testing.T) {
	g := newRungRig(t, "success")
	g.r.store = failingResolve{g.s}
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	steps := g.steps(t, out.CaseID)
	blob, _ := json.Marshal([]any{steps, g.asks(t), g.forge.comments})
	if strings.Contains(string(blob), "user:pass") || strings.Contains(string(blob), "abc123def456ghi789") {
		t.Fatalf("a secret reached the record: %s", blob)
	}
	asks := g.asks(t)
	if len(steps) != 4 || steps[1].Outcome != StepHandedUp || !strings.Contains(steps[1].Found, "could not be carried out") || len(g.forge.comments) != 1 || len(asks) != 1 ||
		!strings.Contains(fmt.Sprint(asks[0].Detail["info"]), "The check-in found:") || !strings.Contains(fmt.Sprint(asks[0].Detail["info"]), "could not be carried out") {
		t.Fatalf("steps %+v, comments %q, asks %+v; want the failure in rung 1's found and one masked face that names it", steps, g.forge.comments, asks)
	}
}

// A live session row is what makes a running card's session run: with one a
// resume is refused, without it the card's stop takes it.
func TestFactsReadASessionRunningOnlyFromItsLiveRow(t *testing.T) {
	g := newRungRig(t, "success")
	ctx := context.Background()
	card, _, _ := g.s.Task(ctx, "t1")
	f, err := g.r.tripFacts(ctx, card, "", "")
	if err != nil || f.Running {
		t.Fatalf("running with no live row = %v, %v, want false", f.Running, err)
	}
	if err := g.s.PutLiveSession(ctx, LiveSession{TaskID: "t1", SessionID: "s1", Namespace: "ns", Pod: "p", MessageToken: "tok", StartedAt: time.Now(), Deadline: time.Now().Add(time.Hour), Owner: "a"}); err != nil {
		t.Fatal(err)
	}
	f, err = g.r.tripFacts(ctx, card, "", "")
	if err != nil || !f.Running || sessionRoom(f) != "a session is running" {
		t.Fatalf("running with a live row = %v (%v), room %q, want true and a session is running", f.Running, err, sessionRoom(f))
	}
}

// Only rung 1 answers as the check-in.
func TestActRefusesTheCheckinOffRungOne(t *testing.T) {
	for _, rung := range []int{0, 2, 3} {
		_, err := (&Runner{}).Act(context.Background(), ActInput{TaskID: "t1", CaseID: 1, By: CheckinDecider, Rung: rung, Res: Resolution{Action: ActExtend}})
		if _, ok := err.(ErrActRefused); !ok || rung != 0 && !strings.Contains(err.Error(), "the check-in answers on rung 1 alone") {
			t.Fatalf("rung %d: err = %v, want the check-in refused off rung 1", rung, err)
		}
	}
}

// A panic in rung 1's conversation stops no process: it is logged masked and
// the case goes to the human with the panic named.
func TestRung1PanicHandsTheCaseUp(t *testing.T) {
	g := newRungRig(t, "success")
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g.chat.onCall = func() { panic("boom: https://user:pass@host/x") }
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	if got := logged.String(); !strings.Contains(got, "rung 1 panicked: boom: https://[redacted]@host/x") || strings.Contains(got, "user:pass") {
		t.Fatalf("log = %q, want the panic logged masked", got)
	}
	steps := g.steps(t, out.CaseID)
	if len(steps) != 4 || steps[1].Outcome != StepHandedUp || !strings.Contains(steps[1].Found, "rung 1 stopped on an internal error: boom") || strings.Contains(steps[1].Found, "user:pass") ||
		caseByID(t, g.s, out.CaseID).Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("steps %+v, comments %d; want rung 1's step handed up naming the panic, masked, and one face", steps, len(g.forge.comments))
	}
}

// The code-written parts of a session's note are never cut: whatever the
// source of the answer's note, a rung's or the human's console note, and
// however long it, the findings, the checks log and the kill, the note fits
// the run's answer bound and the parts stand whole. By hand, the worst case
// of a rung's note: the lead of five findings of 203 bytes (14 + 203 + 19 +
// 4 x 203 + 9 = 1057), the log's 803, the kill line's 403 and three
// separators of 2: 2269 bytes, leaving 1731, of which the cut's mark takes 3.
func TestSessionNoteKeepsItsCodeWrittenPartsWithinTheAnswerBound(t *testing.T) {
	r := &Runner{}
	f := CaseFacts{Head: "0123456789abcdef", Checks: "red", Killed: &SessionKill{Reason: "OOMKilled", Command: strings.Repeat("go test ", 200)}}
	for i := range 20 {
		text := fmt.Sprintf("finding %02d %s", i, strings.Repeat("x", 500))
		id := findingID(text)
		f.Findings, f.FindingOrder = append(f.Findings, id), append(f.FindingOrder, id)
		if f.FindingTexts == nil {
			f.FindingTexts = map[string]string{}
		}
		f.FindingTexts[id] = text
	}
	for i := range 100 {
		f.FailedTests = append(f.FailedTests, FailedTest{Test: fmt.Sprintf("TestNumber%03d", i), Package: "example.com/long/package/path", Kind: "failed"})
	}
	log := checksLog(f, checksLogCut)
	kill := textutil.Truncate("The sandbox killed the last session for its memory while it ran: "+f.Killed.Command, killCut)
	for name, in := range map[string]ActInput{
		"a rung's note": {Rung: 1, Res: Resolution{Action: ActHandover, Args: map[string]any{"model": "same"}, Note: strings.Repeat("n", 9000)}},
		"the human's":   {Rung: 3, Res: Resolution{Action: ActContinue, Note: strings.Repeat("h", 9000)}},
		"a short note":  {Rung: 1, Res: Resolution{Action: ActContinue, Note: "go"}},
		"a rung's draw": {Rung: 1, Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1.0}, Note: strings.Repeat("n", 9000)}},
	} {
		note := r.sessionNote(f, in)
		if len(note) > MaxAnswer || !strings.HasSuffix(note, kill) || !strings.Contains(note, log+"\n\n"+kill) {
			t.Errorf("%s: note is %d bytes (bound %d), want it to end with the whole log and kill line", name, len(note), MaxAnswer)
		}
		if in.Rung == 1 && !strings.HasPrefix(note, "Your one job: finding 00") {
			t.Errorf("%s: note starts %q, want the first finding", name, note[:40])
		}
	}
	long := r.sessionNote(f, ActInput{Rung: 1, Res: Resolution{Action: ActHandover, Note: strings.Repeat("n", 9000)}})
	if want := 1057 + 2 + 1731 + 2 + len(log) + 2 + len(kill); len(long) != want {
		t.Errorf("worst case: %d bytes, want %d (the note takes the room that is left: 1731 less the mark's 3, plus the mark)", len(long), want)
	}
}

// At the due of rung 1's step the sweep ends the step first: the case's wake,
// due at the same time, finds it at the human, so no false repeat of rung 0
// is recorded.
func TestSweepExpiresRungOneBeforeItWakesItsCase(t *testing.T) {
	g := newRungRig(t, "success")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	g.r.now = now
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	ctx := context.Background()
	steps := g.steps(t, out.CaseID)
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 1`, out.CaseID); err != nil {
		t.Fatal(err)
	}
	clock.Store(steps[1].DueAt.Add(time.Minute).UnixNano())
	v := g.r.cfg.Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: now}
	if err := m.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	g.settle()
	g.afterRung2(t, out.CaseID)
	if err := m.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range g.steps(t, out.CaseID) {
		if strings.HasPrefix(s.Found, "the same stop again") {
			t.Fatalf("steps = %+v, want no repeat of rung 0 at rung 1's due", g.steps(t, out.CaseID))
		}
	}
	faces := 0
	for _, a := range g.asks(t) {
		if a.Detail["case_id"] == float64(out.CaseID) {
			faces++
		}
	}
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman || faces != 1 {
		t.Fatalf("case %s with %d faces; want it at the human with one", c.Status, faces)
	}
}

// Rung 1, then rung 2, work on a case when its due and its wake pass:
// the heal pass, before or after the sweep in the same minute, leaves the
// case to the sweep, whose expiry faces it once and tells the run the face's
// comment. The card waits on the human.
func TestAHeldCaseIsFacedAtItsDueWhicheverPassRunsFirst(t *testing.T) {
	for _, healFirst := range []bool{true, false} {
		ctx := context.Background()
		g := newRungRig(t, "success")
		g.chat.answers = []CheckinResponse{g.handUp(t)}
		entered, release := g.holdFirstCall()
		out := g.trip(t, tripCap)
		<-entered
		for _, q := range []string{
			`UPDATE cases SET opened_at = now() - interval '36 minutes', next_wake_at = now() - interval '20 seconds' WHERE id = $1`,
			`UPDATE case_steps SET started_at = now() - interval '35 minutes 20 seconds', due_at = now() - interval '20 seconds' WHERE case_id = $1 AND rung = 1`,
		} {
			if _, err := g.s.db.ExecContext(ctx, q, out.CaseID); err != nil {
				t.Fatal(err)
			}
		}
		v := g.r.cfg.Policy()
		m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: time.Now}
		passes := func() {
			var rep CaseHealReport
			var healErr, sweepErr error
			if healFirst {
				rep, healErr = g.s.ReconcileCases(ctx, "demo-app/demo-app")
				sweepErr = m.Sweep(ctx)
			} else {
				sweepErr = m.Sweep(ctx)
				rep, healErr = g.s.ReconcileCases(ctx, "demo-app/demo-app")
			}
			if healErr != nil || sweepErr != nil || rep.Moved != 0 {
				t.Fatalf("heal first %v: heal %+v (%v), sweep %v; want nothing moved", healFirst, rep, healErr, sweepErr)
			}
		}
		// rung 1's bound ends: the case climbs to rung 2, with no face yet
		passes()
		close(release)
		g.settle()
		for _, q := range []string{
			`UPDATE cases SET next_wake_at = now() - interval '20 seconds' WHERE id = $1`,
			`UPDATE case_steps SET started_at = now() - interval '35 minutes 20 seconds', due_at = now() - interval '20 seconds' WHERE case_id = $1 AND rung = 2`,
		} {
			if _, err := g.s.db.ExecContext(ctx, q, out.CaseID); err != nil {
				t.Fatal(err)
			}
		}
		// rung 2's bound ends: the sweep's expiry faces it once
		passes()
		faces := 0
		for _, a := range g.asks(t) {
			if a.Detail["case_id"] == float64(out.CaseID) {
				faces++
			}
		}
		c := caseByID(t, g.s, out.CaseID)
		card, _, _ := g.s.Task(ctx, "t1")
		faced := g.signalsOf(SignalCaseFaced)
		if c.Status != CaseAtHuman || faces != 1 || len(faced) != 1 || faced[0].sig.Case.Comment != c.CommentID || card.Status != "waiting-human" {
			t.Fatalf("heal first %v: case %s, %d faces, signals %+v, card %s; want the case at the human with one face, its comment signalled, the card waiting",
				healFirst, c.Status, faces, faced, card.Status)
		}
	}
}

// The sweep's face of an expired rung 2 step fails: the case keeps its wake,
// the heal pass leaves it, and the next sweep faces it.
func TestAFailedFaceAtTheDueKeepsTheWake(t *testing.T) {
	ctx := context.Background()
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	entered, release := g.holdFirstCall()
	out := g.trip(t, tripCap)
	<-entered
	for _, q := range []string{
		`UPDATE cases SET opened_at = now() - interval '36 minutes', next_wake_at = now() - interval '20 seconds' WHERE id = $1`,
		`UPDATE case_steps SET started_at = now() - interval '35 minutes 20 seconds', due_at = now() - interval '20 seconds' WHERE case_id = $1 AND rung = 1`,
	} {
		if _, err := g.s.db.ExecContext(ctx, q, out.CaseID); err != nil {
			t.Fatal(err)
		}
	}
	faces := func() int {
		n := 0
		for _, a := range g.asks(t) {
			if a.Detail["case_id"] == float64(out.CaseID) {
				n++
			}
		}
		return n
	}
	// rung 1's bound ends and the case climbs to rung 2, which has no face yet
	first, err := g.s.ExpireSteps(ctx, time.Now())
	if err != nil || len(first) != 1 {
		t.Fatalf("expired %+v (%v), want rung 1's step", first, err)
	}
	if err := g.r.climbExpired(ctx, first); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE cases SET next_wake_at = now() - interval '20 seconds' WHERE id = $1`,
		`UPDATE case_steps SET started_at = now() - interval '35 minutes 20 seconds', due_at = now() - interval '20 seconds' WHERE case_id = $1 AND rung = 2`,
	} {
		if _, err := g.s.db.ExecContext(ctx, q, out.CaseID); err != nil {
			t.Fatal(err)
		}
	}
	g.forge.failComment = true
	expired, err := g.s.ExpireSteps(ctx, time.Now())
	if err != nil || len(expired) != 1 || expired[0].Rung != 2 {
		t.Fatalf("expired %+v (%v), want rung 2's step", expired, err)
	}
	if err := g.r.climbExpired(ctx, expired); err == nil {
		t.Fatal("the expiry's face failed, want its error")
	}
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseOpen || c.NextWakeAt == nil || faces() != 0 {
		t.Fatalf("after the failed face: case %s, wake %v, %d faces; want it open with its wake and no face", c.Status, c.NextWakeAt, faces())
	}
	if rep, err := g.s.ReconcileCases(ctx, "demo-app/demo-app"); err != nil || rep.Moved != 0 {
		t.Fatalf("heal %+v (%v), want nothing moved", rep, err)
	}
	v := g.r.cfg.Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: time.Now}
	if err := m.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	g.settle()
	faced := g.signalsOf(SignalCaseFaced)
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman || faces() != 1 || len(faced) != 1 || faced[0].sig.Case.CaseID != out.CaseID {
		t.Fatalf("after the next sweep: case %s, %d faces, signals %+v; want it at the human, one face, its signal", c.Status, faces(), faced)
	}
}

// A case that climbs from rung 0 after rung 1 judged it already (a face a
// failed hand-up left for the monitor) still carries rung 1's recommendation;
// a moment's human answer carries none.
func TestHandedUpByKeepsRungOnesRecommendation(t *testing.T) {
	steps := []CaseStep{{Rung: 0, Outcome: StepHandedUp}, {Rung: 1, Outcome: StepHandedUp, Decision: "close", Grounds: "done on main"}}
	if up := handedUpBy(steps, 0, "f"); up == nil || up.Action != "close" || up.Grounds != "done on main" {
		t.Fatalf("hand-up = %+v, want rung 1's close", up)
	}
	moment := []CaseStep{{Rung: 1, Outcome: StepHandedUp, Decision: RungHandUp, Grounds: "goal conflict"}}
	if up := handedUpBy(moment, 0, ""); up == nil || up.Action != "" || up.Found != "goal conflict" {
		t.Fatalf("a moment's hand-up = %+v, want no recommendation and its reason found", up)
	}
	if up := handedUpBy([]CaseStep{{Rung: 0, Outcome: StepHandedUp}}, 0, ""); up != nil {
		t.Fatalf("hand-up of rung 0 alone = %+v, want none", up)
	}
}

// A face that failed to post when rung 2's bound ended leaves the case open,
// and the monitor posts the face when the case's wake comes: the face carries
// what rung 1 found and recommended, not rung 0's "no resolver fits", and the
// run is told the face's comment, as the trip returned none.
func TestAFaceTheMonitorRecoversCarriesRungOnesFindingAndSignalsTheRun(t *testing.T) {
	g := newRungRig(t, "success")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	g.r.now = now
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	g.forge.failComment = true
	v := g.r.cfg.Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: now}
	own := g.stepsOfRung(t, out.CaseID, 2)
	clock.Store(own[0].DueAt.Add(time.Minute).UnixNano())
	// rung 2's bound ends and its face fails to post once; the sweep's due
	// wake of the case posts it
	if err := m.Sweep(context.Background()); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Sweep = %v, want the failed post of the first face named", err)
	}
	g.settle()
	c := caseByID(t, g.s, out.CaseID)
	faced := g.signalsOf(SignalCaseFaced)
	var info, recommendation string
	for _, a := range g.asks(t) {
		if a.Detail["case_id"] == float64(out.CaseID) {
			info, recommendation = fmt.Sprint(a.Detail["info"]), fmt.Sprint(a.Detail["recommendation"])
		}
	}
	if c.Status != CaseAtHuman || c.CommentID == 0 || len(faced) != 1 || faced[0].sig.Case.Comment != c.CommentID {
		t.Fatalf("case %s with comment %d, signals %+v; want it at the human and one case_faced with that comment", c.Status, c.CommentID, faced)
	}
	if !strings.Contains(info, "The check-in found: the stop reads as gate_stop; finding") || strings.Contains(info, "no resolver fits") ||
		!strings.Contains(recommendation, "close") || !strings.Contains(recommendation, "the card's work is done on main") {
		t.Fatalf("info %q, recommendation %q; want rung 1's finding and recommendation", info, recommendation)
	}
}

// A check-in moment's face shows the moment's reason as what the check-in
// found, not rung 0's hand-up.
func TestAMomentsFaceShowsItsReasonNotRungZeros(t *testing.T) {
	g := newRungRig(t, "success")
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: "the objective conflicts with the guardrails", Kind: CaseCheckinHuman,
		StateKey: "k", Extra: map[string]any{"reason_kind": CheckGoalConflict}}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.settle()
	g.afterRung2(t, out.CaseID)
	var info string
	for _, a := range g.asks(t) {
		if a.Detail["case_id"] == float64(out.CaseID) {
			info = fmt.Sprint(a.Detail["info"])
		}
	}
	if !strings.Contains(info, "The check-in found: the objective conflicts with the guardrails") || strings.Contains(info, "no resolver fits") {
		t.Fatalf("info = %q, want the moment's reason as what the check-in found", info)
	}
}

// A red head whose status names no Actions run leaves the checks log unread:
// the facts say so, and a head whose run names its failed test does not.
func TestFactsMarkAnUnreadChecksLogPartial(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		log     string
		partial bool
	}{{"", true}, {logLines("--- FAIL: TestSlugTrims (0.00s)", "FAIL\texample.com/slug\t1s"), false}} {
		g := newRungRig(t, "failure")
		g.r.store = projectStore{g.s}
		g.forge.redLog = tc.log
		card, _, _ := g.s.Task(ctx, "t1")
		f, err := g.r.tripFacts(ctx, card, "", "")
		if err != nil || f.Checks != "red" || slices.Contains(f.Partial, "checks_log") != tc.partial || (len(f.FailedTests) == 0) != tc.partial {
			t.Fatalf("log %q: checks %q, partial %v, failed tests %+v (%v); want partial %v", tc.log, f.Checks, f.Partial, f.FailedTests, err, tc.partial)
		}
	}
}

type flakySignals struct {
	*caseSignals
	fails int
	at    []time.Time
}

func (w *flakySignals) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	w.at = append(w.at, time.Now())
	if len(w.at) <= w.fails {
		return fmt.Errorf("signal refused: https://user:pass@host")
	}
	return w.caseSignals.Signal(ctx, taskID, name, sig)
}

// A case_faced signal that fails is sent again, with a doubling pause, up to
// four tries: two failures then success deliver it once; failures past the
// bound give up; a context that ends stops it; the refusals are logged masked.
func TestSignalFacedRetriesWithinItsBound(t *testing.T) {
	old := facedBackoff
	facedBackoff = 20 * time.Millisecond
	t.Cleanup(func() { facedBackoff = old })
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	signals := &caseSignals{}
	flaky := &flakySignals{caseSignals: signals, fails: 2}
	(&Runner{store: &fakeStore{}, cfg: RunnerConfig{Workflows: flaky}}).signalFaced(context.Background(), "t1", 5, 640)
	if len(flaky.at) != 3 || len(signals.got) != 1 || signals.got[0].signal != SignalCaseFaced || signals.got[0].sig.Case.Comment != 640 {
		t.Fatalf("tries %d, delivered %+v; want the third try delivering once", len(flaky.at), signals.got)
	}
	if first, second := flaky.at[1].Sub(flaky.at[0]), flaky.at[2].Sub(flaky.at[1]); first < 20*time.Millisecond || second < 40*time.Millisecond {
		t.Fatalf("pauses %v and %v, want at least 20ms then 40ms: the pause doubles", first, second)
	}
	if got := logged.String(); !strings.Contains(got, "signal refused: https://[redacted]@host") || strings.Contains(got, "user:pass") {
		t.Fatalf("log = %q, want the refusal logged masked", got)
	}

	never := &flakySignals{caseSignals: &caseSignals{}, fails: 100}
	(&Runner{cfg: RunnerConfig{Workflows: never}}).signalFaced(context.Background(), "t1", 5, 640)
	if len(never.at) != 4 {
		t.Fatalf("tries %d, want 4", len(never.at))
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := &flakySignals{caseSignals: &caseSignals{}, fails: 100}
	(&Runner{cfg: RunnerConfig{Workflows: stopped}}).signalFaced(ended, "t1", 5, 640)
	if len(stopped.at) != 1 {
		t.Fatalf("tries %d with the context ended, want the one", len(stopped.at))
	}
}

// The face's signal runs off the sweep's path and outlives it: the sweep that
// faces an expired rung 1 step returns while the signal hangs, and the signal
// lands after the sweep's context ended.
func TestAFaceDoesNotWaitForItsSignal(t *testing.T) {
	g := newRungRig(t, "success")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	g.r.now = now
	held := make(chan struct{})
	g.r.cfg.Workflows = heldSignals{Workflows: g.r.cfg.Workflows, release: held}
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	steps := g.steps(t, out.CaseID)
	clock.Store(steps[1].DueAt.Add(time.Minute).UnixNano())
	v := g.r.cfg.Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: now}
	// rung 1's bound ends first; rung 2 holds the case, and its own bound ends in the sweep under test
	if err := m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	own := g.stepsOfRung(t, out.CaseID, 2)
	if len(own) != 1 {
		t.Fatalf("rung 2 steps %+v, want rung 2 to hold the case", own)
	}
	clock.Store(own[0].DueAt.Add(time.Minute).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Sweep(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("sweep: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("the sweep waited for the face's signal")
		cancel()
		close(held)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the sweep still waits with the signal released and its context ended")
		}
		close(release)
		g.settle()
		return
	}
	cancel()
	close(held)
	close(release)
	g.settle()
	if faced := g.signalsOf(SignalCaseFaced); len(faced) != 1 || faced[0].sig.Case.CaseID != out.CaseID {
		t.Fatalf("signals %+v, want the face's signal delivered after the sweep's context ended", faced)
	}
}

// heldSignals holds every signal until release closes, then delivers it, or
// drops it when its context ends first.
type heldSignals struct {
	Workflows
	release chan struct{}
}

func (h heldSignals) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	select {
	case <-h.release:
		return h.Workflows.Signal(ctx, taskID, name, sig)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A human's console note on a card whose red checks add a log is cut to the
// handover cap, the log kept whole after it, and the issue's record puts the
// human's words and factoryd's additions apart under their own names.
func TestAHumansNoteIsCutAtTheCapAndFactorydsAdditionsAreItsOwn(t *testing.T) {
	g := newRungRig(t, "failure")
	g.r.store = projectStore{g.s}
	g.forge.redLog = logLines("--- FAIL: TestSlugTrims (0.00s)", "FAIL\texample.com/slug\t1s")
	g.trip(t, tripCap)
	second := g.trip(t, tripCap)
	if second.CaseID == 0 {
		t.Fatal("no case")
	}
	g.settle()
	long := strings.Repeat("h", 3000)
	if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: second.CaseID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActContinue, Note: long}}); err != nil {
		t.Fatal(err)
	}
	c := caseByID(t, g.s, second.CaseID)
	note := c.Resolution.Reason
	tail := "The checks on h2 are red:\n- TestSlugTrims (example.com/slug): failed"
	if !strings.HasSuffix(note, tail) || !strings.HasPrefix(note, strings.Repeat("h", 2000)+"…\n\n") {
		t.Fatalf("note = %.60q…%q, want the human's words cut at the 2000 cap, then the log", note, note[max(0, len(note)-80):])
	}
	var posted struct{ Body string }
	if err := json.Unmarshal([]byte(g.forge.comments[len(g.forge.comments)-1]), &posted); err != nil {
		t.Fatal(err)
	}
	comment := posted.Body
	if !strings.Contains(comment, "factoryd: sebastian answered with continue") || !strings.Contains(comment, strings.Repeat("h", 3000)) ||
		!strings.Contains(comment, "factoryd added for the session:") || strings.Index(comment, tail) < strings.Index(comment, "factoryd added for the session:") {
		t.Fatalf("comment = %.200q, want the human's whole note under the human's name and the log under factoryd's", comment)
	}
}

// The run ends a case whose face it may not know of: the face is withdrawn
// as WithdrawQuestion withdraws a question, with one note naming why, masked,
// the card back in review and the inbox's row settled as withdrawn. A note
// that fails to post fails the end, and the retry posts it; an end again
// posts none. A case with no face ends with no note.
func TestEndCaseWithdrawsAPostedFaceWithOneNote(t *testing.T) {
	const url, bearer = "https://user:pass@host/x", "Authorization: Bearer abc123def456ghi789"
	ctx := context.Background()
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	faced := caseByID(t, g.s, out.CaseID)
	if card, _, _ := g.s.Task(ctx, "t1"); faced.Status != CaseAtHuman || len(g.forge.comments) != 1 || card.Status != "waiting-human" {
		t.Fatalf("case %s with %d comments, card %s; want it faced, the card waiting", faced.Status, len(g.forge.comments), card.Status)
	}
	why := "reviewer sebastian requested changes at " + url + " with " + bearer
	g.forge.failComment = true
	if err := g.r.EndCase(ctx, faced.ID, why); err == nil || caseByID(t, g.s, faced.ID).Status != CaseAtHuman {
		t.Fatalf("an end whose note failed: %v, case %s; want the failure and the case at the human", err, caseByID(t, g.s, faced.ID).Status)
	}
	for range 2 {
		if err := g.r.EndCase(ctx, faced.ID, why); err != nil {
			t.Fatal(err)
		}
	}
	if c := caseByID(t, g.s, faced.ID); c.Status != CaseWithdrawn || len(g.forge.comments) != 2 {
		t.Fatalf("case %s with %d comments, want it withdrawn with one note after the face", c.Status, len(g.forge.comments))
	}
	var note struct{ Body string }
	if err := json.Unmarshal([]byte(g.forge.comments[1]), &note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note.Body, "withdraws its question above because reviewer sebastian requested changes") ||
		strings.Contains(note.Body, "user:pass") || strings.Contains(note.Body, "abc123def456ghi789") {
		t.Fatalf("note = %q, want the reason, masked", note.Body)
	}
	card, _, _ := g.s.Task(ctx, "t1")
	events, err := g.s.TaskEvents(ctx, "t1", 200)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(events)
	asks := g.asks(t)
	row := SettleOf(ChatMessage{Kind: MessageEscalation, BirthKind: EventQuestionAsked, At: asks[len(asks)-1].At}, events, card.Status, false)
	withdrawn := g.rows(t, EventQuestionWithdrawn)
	if card.Status != "in-review" || row.Open || row.Word != "withdrawn" || len(withdrawn) != 1 || withdrawn[0].Detail["case_id"] != float64(faced.ID) {
		t.Fatalf("card %s, inbox row %+v, withdrawn rows %+v; want the card in review, the row settled withdrawn by one row naming case %d",
			card.Status, row, withdrawn, faced.ID)
	}

	g2 := newRungRig(t, "success")
	release := make(chan struct{})
	g2.chat.onCall = func() { <-release }
	g2.chat.answers = []CheckinResponse{g2.handUp(t)}
	held := g2.trip(t, tripCap)
	if err := g2.r.EndCase(ctx, held.CaseID, "the card went back to the planner"); err != nil {
		t.Fatal(err)
	}
	close(release)
	g2.settle()
	if c := caseByID(t, g2.s, held.CaseID); c.Status != CaseWithdrawn || len(g2.forge.comments) != 0 {
		t.Fatalf("case %s with %d comments, want it withdrawn with no note", c.Status, len(g2.forge.comments))
	}
}

// What factoryd adds to a note for the session is masked: the kill line of a
// session the sandbox killed, its command carrying URL user info and a bearer
// value, reaches a human's answer's comment masked, and so do a checks log
// whose job names carry them and the finding that leads rung 1's note.
func TestFactorydsAdditionsToANoteAreMasked(t *testing.T) {
	const url, bearer = "https://user:pass@host/x", "Authorization: Bearer abc123def456ghi789"
	masked := func(t *testing.T, what, s string) {
		t.Helper()
		if strings.Contains(s, "user:pass") || strings.Contains(s, "abc123def456ghi789") || !strings.Contains(s, "Bearer [redacted]") {
			t.Fatalf("%s = %q, want both secrets masked", what, s)
		}
	}
	ctx := context.Background()
	g := newRungRig(t, "success")
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap, Killed: &SessionKill{Reason: "OOMKilled", ExitCode: 137,
		Command: "curl " + url + " -H '" + bearer + "'"}}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.settle()
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActContinue, Note: "go on"}}); err != nil {
		t.Fatal(err)
	}
	var posted struct{ Body string }
	if err := json.Unmarshal([]byte(g.forge.comments[len(g.forge.comments)-1]), &posted); err != nil {
		t.Fatal(err)
	}
	added := posted.Body[strings.Index(posted.Body, "factoryd added for the session:"):]
	if !strings.Contains(added, "The sandbox killed the last session for its memory") {
		t.Fatalf("comment = %q, want the kill line among factoryd's additions", posted.Body)
	}
	masked(t, "the comment's additions", added)

	f := CaseFacts{Checks: "red", Head: "h2", FailedTests: []FailedTest{{Job: "test " + url + " " + bearer, Kind: "no test named"}}}
	_, after := g.r.sessionParts(f, ActInput{Rung: 3, Res: Resolution{Action: ActContinue}})
	if len(after) != 1 || !strings.HasPrefix(after[0], "The checks on h2 are red:") {
		t.Fatalf("parts after the note = %q, want the checks log", after)
	}
	masked(t, "the checks log", after[0])

	f = CaseFacts{Findings: []string{"f1"}, FindingTexts: map[string]string{"f1": "the client logs " + url + " with " + bearer}}
	before, _ := g.r.sessionParts(f, ActInput{Rung: 1, Res: Resolution{Action: ActContinue}})
	if len(before) != 1 || !strings.HasPrefix(before[0], "Your one job: the client logs") {
		t.Fatalf("parts before rung 1's note = %q, want the finding as the one job", before)
	}
	masked(t, "rung 1's lead", before[0])
}

// CaseState reads the store: a case rung 1 holds has no face, a faced one
// its comment, and an ended one says so.
func TestCaseStateReadsTheStore(t *testing.T) {
	ctx := context.Background()
	g := newRungRig(t, "success")
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	held, err := g.r.CaseState(ctx, out.CaseID)
	close(release)
	g.settle()
	g.afterRung2(t, out.CaseID)
	faced, err2 := g.r.CaseState(ctx, out.CaseID)
	comment := caseByID(t, g.s, out.CaseID).CommentID
	if err := g.s.WithdrawCase(ctx, out.CaseID, "the card moved on"); err != nil {
		t.Fatal(err)
	}
	ended, err3 := g.r.CaseState(ctx, out.CaseID)
	if err != nil || err2 != nil || err3 != nil || held != (CaseState{}) || comment == 0 || faced != (CaseState{Comment: comment}) || ended != (CaseState{Comment: comment, Ended: true}) {
		t.Fatalf("held %+v, faced %+v, ended %+v (%v %v %v), comment %d; want none, the face's comment, then ended", held, faced, ended, err, err2, err3, comment)
	}
}

// A case's signal is pending in the store until a send delivers it: one
// delivered at once leaves nothing pending, and one whose send failed the
// heal pass sends again. Rung 1's own answer, a human's console answer while
// rung 1 holds the case, and a face whose four tries all failed each reach
// the run once, with what they announce, and leave nothing pending.
func TestCaseSignalsWaitInTheStoreUntilASendDelivers(t *testing.T) {
	old := facedBackoff
	facedBackoff = time.Millisecond
	t.Cleanup(func() { facedBackoff = old })
	ctx := context.Background()
	later := func() time.Time { return time.Now().Add(2 * caseSignalWait) }
	pending := func(t *testing.T, g rungRig) []PendingSignal {
		t.Helper()
		p, err := g.s.PendingCaseSignals(ctx, later(), 10)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		fails int
		act   func(t *testing.T, g rungRig) int64
		want  string
	}{
		{"rung 1's answer delivered", 0, func(t *testing.T, g rungRig) int64 {
			g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
			out := g.trip(t, tripCap)
			g.settle()
			return out.CaseID
		}, SignalCaseResolved},
		{"a face delivered", 0, func(t *testing.T, g rungRig) int64 {
			g.chat.answers = []CheckinResponse{g.handUp(t)}
			out := g.trip(t, tripCap)
			g.settle()
			g.afterRung2(t, out.CaseID)
			return out.CaseID
		}, SignalCaseFaced},
		{"rung 1's answer", 1, func(t *testing.T, g rungRig) int64 {
			g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
			out := g.trip(t, tripCap)
			g.settle()
			return out.CaseID
		}, SignalCaseResolved},
		{"a console answer while rung 1 holds", 1, func(t *testing.T, g rungRig) int64 {
			release := make(chan struct{})
			g.chat.onCall = func() { <-release }
			g.chat.answers = []CheckinResponse{g.handUp(t)}
			out := g.trip(t, tripCap)
			if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaConsole, Rung: 3,
				Res: Resolution{Action: ActContinue, Note: "go on"}}); err != nil {
				t.Fatal(err)
			}
			close(release)
			g.settle()
			return out.CaseID
		}, SignalCaseResolved},
		{"a face", facedTries, func(t *testing.T, g rungRig) int64 {
			g.chat.answers = []CheckinResponse{g.handUp(t)}
			out := g.trip(t, tripCap)
			g.settle()
			g.afterRung2(t, out.CaseID)
			return out.CaseID
		}, SignalCaseFaced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRungRig(t, "success")
			g.r.cfg.Workflows = &flakySignals{caseSignals: g.signals, fails: tc.fails}
			id := tc.act(t, g)
			if tc.fails == 0 {
				if got, p := g.signalsOf(tc.want), pending(t, g); len(got) != 1 || len(p) != 0 {
					t.Fatalf("%s delivered %+v, pending %+v; want one delivered and nothing pending", tc.want, got, p)
				}
				return
			}
			if got := g.signalsOf(tc.want); len(got) != 0 {
				t.Fatalf("%s delivered %+v before the heal pass, want every send failed", tc.want, got)
			}
			if p := pending(t, g); len(p) != 1 || p[0].CaseID != id || p[0].Name != tc.want || p[0].Signal.Case == nil || p[0].Signal.Case.CaseID != id {
				t.Fatalf("pending %+v, want case %d's %s", p, id, tc.want)
			}
			resendCaseSignals(ctx, g.s, g.r.cfg.Workflows, later())
			got := g.signalsOf(tc.want)
			c := caseByID(t, g.s, id)
			if len(got) != 1 || got[0].sig.Case.CaseID != id || len(pending(t, g)) != 0 {
				t.Fatalf("%s delivered %+v, pending %+v; want one for case %d and nothing pending", tc.want, got, pending(t, g), id)
			}
			switch {
			case tc.want == SignalCaseFaced && got[0].sig.Case.Comment != c.CommentID:
				t.Fatalf("case_faced carries comment %d, want the face's %d", got[0].sig.Case.Comment, c.CommentID)
			case tc.want == SignalCaseResolved && got[0].sig.Case.Res.Action != c.Resolution.Action:
				t.Fatalf("case_resolved carries %q, want the case's %q", got[0].sig.Case.Res.Action, c.Resolution.Action)
			}
		})
	}
}

// The heal pass's sends of a pending signal are bounded: each failure is
// logged masked and counted, and the row ends after caseSignalTries. A row
// of a card whose run is over ends with no send.
func TestTheHealPassBoundsItsSendsOfACaseSignal(t *testing.T) {
	ctx := context.Background()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g := newRungRig(t, "success")
	g.r.cfg.Workflows = &flakySignals{caseSignals: g.signals, fails: 1000}
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	out := g.trip(t, tripCap)
	g.settle()
	later := time.Now().Add(2 * caseSignalWait)
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_signals SET tries = $2 WHERE case_id = $1`, out.CaseID, caseSignalTries-2); err != nil {
		t.Fatal(err)
	}
	resendCaseSignals(ctx, g.s, g.r.cfg.Workflows, later)
	if p, _ := g.s.PendingCaseSignals(ctx, later, 10); len(p) != 1 || p[0].Tries != caseSignalTries-1 {
		t.Fatalf("pending %+v after a failed send, want the row with %d tries", p, caseSignalTries-1)
	}
	resendCaseSignals(ctx, g.s, g.r.cfg.Workflows, later)
	if p, _ := g.s.PendingCaseSignals(ctx, later, 10); len(p) != 0 {
		t.Fatalf("pending %+v after the last try, want none", p)
	}
	if got := logged.String(); !strings.Contains(got, "https://[redacted]@host") || strings.Contains(got, "user:pass") ||
		!strings.Contains(got, fmt.Sprintf("no send delivered it in %d tries", caseSignalTries)) {
		t.Fatalf("log = %q, want the failures masked and the end named", got)
	}

	g2 := newRungRig(t, "success")
	g2.r.cfg.Workflows = &flakySignals{caseSignals: g2.signals, fails: 1}
	g2.chat.answers = []CheckinResponse{g2.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	g2.trip(t, tripCap)
	g2.settle()
	if _, err := g2.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	resendCaseSignals(ctx, g2.s, g2.r.cfg.Workflows, later)
	if p, _ := g2.s.PendingCaseSignals(ctx, later, 10); len(p) != 0 || len(g2.signalsOf(SignalCaseResolved)) != 0 {
		t.Fatalf("pending %+v, delivered %d; want the ended card's row gone with no send", p, len(g2.signalsOf(SignalCaseResolved)))
	}
}

// failingWake fails the first write of a wake with a time.
type failingWake struct {
	Store
	fails int
}

func (s *failingWake) SetCaseWake(ctx context.Context, id int64, rung int, on []string, at *time.Time) error {
	if s.fails > 0 && at != nil {
		s.fails--
		return errors.New("connection reset")
	}
	return s.Store.SetCaseWake(ctx, id, rung, on, at)
}

// Rung 1's step and its case's wake land together: a store whose separate
// wake write would fail once leaves no step without a wake, so a retried trip
// finds rung 1 at work on a case the sweep wakes, and the heal leaves it to
// the sweep past ten minutes; one conversation runs.
func TestRungOnesStepNeverHoldsACaseWithoutAWake(t *testing.T) {
	ctx := context.Background()
	g := newRungRig(t, "success")
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	g.r.store = &failingWake{Store: g.s, fails: 1}
	_, err1 := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap}, tripAttempt{N: 1})
	out, err2 := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap}, tripAttempt{N: 2})
	if err2 != nil {
		t.Fatalf("trips: %v, %v", err1, err2)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE cases SET opened_at = now() - interval '11 minutes' WHERE id = $1`, out.CaseID); err != nil {
		t.Fatal(err)
	}
	rep, err := g.s.ReconcileCases(ctx, "demo-app/demo-app")
	c := caseByID(t, g.s, out.CaseID)
	close(release)
	g.settle()
	if err != nil || rep.Moved != 0 || c.Status != CaseOpen || c.OwnerRung != 1 || c.NextWakeAt == nil || g.chat.calls != 1 {
		t.Fatalf("heal %+v (%v), case %s owner %d wake %v, conversations %d; want the case rung 1's with its wake, left to the sweep, one conversation",
			rep, err, c.Status, c.OwnerRung, c.NextWakeAt, g.chat.calls)
	}
}

// A card that cannot wait for a human gets no face: a case rung 1 holds past
// its due on a merged card is neither faced nor woken by the sweep, the heal
// withdraws it, and no comment reaches the issue over three minutes of
// passes; a face asked of a merged card ends its case with no comment.
func TestNoFaceOnACardThatCannotWaitForAHuman(t *testing.T) {
	ctx := context.Background()
	g := newRungRig(t, "success")
	release := make(chan struct{})
	g.chat.onCall = func() { <-release }
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	for _, q := range []string{
		`UPDATE cases SET opened_at = now() - interval '36 minutes', next_wake_at = now() - interval '20 seconds' WHERE id = $1`,
		`UPDATE case_steps SET started_at = now() - interval '35 minutes 20 seconds', due_at = now() - interval '20 seconds' WHERE case_id = $1 AND rung = 1`,
	} {
		if _, err := g.s.db.ExecContext(ctx, q, out.CaseID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'merged', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	v := g.r.cfg.Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: time.Now}
	for range 3 {
		if err := m.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := g.s.ReconcileCases(ctx, "demo-app/demo-app"); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	g.settle()
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseWithdrawn || c.Resolution.Reason != "the card moved on to merged" || len(g.forge.comments) != 0 || len(g.asks(t)) != 0 {
		t.Fatalf("case %s (%+v), %d comments, %d asks; want it withdrawn by the heal pass with no face", c.Status, c.Resolution, len(g.forge.comments), len(g.asks(t)))
	}

	g2 := newRungRig(t, "success")
	g2.r.checkin = nil
	c, _, err := g2.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseSessionCap, StateKey: "k", Problem: capAsk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g2.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'merged', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	card, _, _ := g2.s.Task(ctx, "t1")
	if _, err := g2.r.face(ctx, card, c, tripCap, "next", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g2.s, c.ID); got.Status != CaseWithdrawn || len(g2.forge.comments) != 0 {
		t.Fatalf("case %s with %d comments, want it withdrawn with no comment", got.Status, len(g2.forge.comments))
	}
}

// downSignals fails every send: as a frontend that is down when retryable,
// else as one that refuses the signal.
type downSignals struct {
	Workflows
	retryable bool
}

func (d downSignals) Signal(context.Context, string, string, TaskSignal) error {
	if d.retryable {
		return serviceerror.NewUnavailable("frontend down at https://user:pass@host")
	}
	return errors.New("signal refused: https://user:pass@host")
}

// A pending case signal never ends while its sends fail for a frontend that
// is down, however long; a failing row logs one line in caseSignalLogEvery
// failed sends, masked; and once the frontend refuses it for good, a row
// that failed caseSignalTries times ends.
func TestTheHealPassNeverDropsASignalForAFrontendThatIsDown(t *testing.T) {
	ctx := context.Background()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g := newRungRig(t, "success")
	g.r.cfg.Workflows = downSignals{Workflows: g.signals, retryable: true}
	g.chat.answers = []CheckinResponse{g.answer(t, map[string]any{"decision": "extend", "draw": map[string]any{"sessions": 1}})}
	out := g.trip(t, tripCap)
	g.settle()
	later := time.Now().Add(2 * caseSignalWait)
	tries := func() int {
		p, err := g.s.PendingCaseSignals(ctx, later, 10)
		if err != nil || len(p) != 1 || p[0].CaseID != out.CaseID {
			t.Fatalf("pending %+v (%v), want case %d's row", p, err, out.CaseID)
		}
		return p[0].Tries
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_signals SET tries = $2 WHERE case_id = $1`, out.CaseID, caseSignalTries+1); err != nil {
		t.Fatal(err)
	}
	for range caseSignalLogEvery {
		resendCaseSignals(ctx, g.s, g.r.cfg.Workflows, later)
	}
	if n := tries(); n != caseSignalTries+1+caseSignalLogEvery {
		t.Fatalf("tries %d, want every failed send counted and the row kept", n)
	}
	lines := strings.Count(logged.String(), fmt.Sprintf("case %d's %s", out.CaseID, SignalCaseResolved))
	if got := logged.String(); lines != 1 || !strings.Contains(got, "https://[redacted]@host") || strings.Contains(got, "user:pass") {
		t.Fatalf("log = %q, want one masked line for %d failed sends", got, caseSignalLogEvery)
	}
	resendCaseSignals(ctx, g.s, downSignals{Workflows: g.signals}, later)
	if p, _ := g.s.PendingCaseSignals(ctx, later, 10); len(p) != 0 || !strings.Contains(logged.String(), "it ends: signal refused: https://[redacted]@host") {
		t.Fatalf("pending %+v, log %q; want the refused row ended, its refusal logged masked", p, logged.String())
	}
}

// A case faced again, as a restart's trip faces it, while its first face's
// signal is still pending sends the newest face: the pending row holds the
// face's newest comment.
func TestAFacedAgainCaseSendsItsNewestFace(t *testing.T) {
	old := facedBackoff
	facedBackoff = time.Millisecond
	t.Cleanup(func() { facedBackoff = old })
	ctx := context.Background()
	g := newRungRig(t, "success")
	g.r.cfg.Workflows = &flakySignals{caseSignals: g.signals, fails: facedTries}
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	c := caseByID(t, g.s, out.CaseID)
	first := c.CommentID
	card, _, _ := g.s.Task(ctx, "t1")
	again, err := g.r.face(ctx, card, c, healedReason, "next", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resendCaseSignals(ctx, g.s, g.r.cfg.Workflows, time.Now().Add(2*caseSignalWait))
	faced := g.signalsOf(SignalCaseFaced)
	if again.Comment == first || len(faced) != 1 || faced[0].sig.Case.Comment != again.Comment {
		t.Fatalf("faces %d then %d, delivered %+v; want the newest face's comment sent", first, again.Comment, faced)
	}
}

// A re-verify of a head the card has had reviewed pays for no second
// verdict: the earlier review comes back as it was. By hand: atHead records
// a reviewed model_review of h2, so a review of h2 with that review earlier
// is the earlier one; of another head it is not (the model is asked, and
// with none configured the call fails); one the check-in's verdicts ride is
// asked again.
func TestModelReviewOfTheSameHeadReusesTheNewestVerdict(t *testing.T) {
	g := newTripRig(t, "success")
	g.atHead(t, false)
	earlier := []Review{{Commit: "h2", Verdict: "concerns", Summary: "one fault", Findings: []string{"main.go: check the flag"}}}
	ctx := context.Background()
	got, err := g.r.ModelReview(ctx, "t1", PullRequest{Number: 9}, "h2", GateResults{}, earlier, 2)
	if err != nil || !reflect.DeepEqual(got, earlier[0]) {
		t.Fatalf("the review of h2 = %+v, %v; want the earlier one", got, err)
	}
	if _, ok := g.r.reviewOfTheHead(ctx, "t1", "h3", earlier); ok {
		t.Error("a head the review did not judge reused it")
	}
	// a retry after a lost result on head B finds B's row and head A's review
	// earlier: it never returns A's verdict
	other := []Review{{Commit: "h1", Verdict: "approve", Summary: "head A's"}}
	if _, ok := g.r.reviewOfTheHead(ctx, "t1", "h2", other); ok {
		t.Error("another head's review was reused for h2")
	}
	if _, ok := g.r.reviewOfTheHead(ctx, "t1", "h2", []Review{{Verdict: "concerns"}}); ok {
		t.Error("a review that names no head was reused")
	}
	dropped := []Review{{Commit: "h2", Verdict: "concerns", Verdicts: map[string]CheckVerdict{"main.go: check the flag": {State: CheckDropped, Evidence: "main.go:3"}}}}
	if _, ok := g.r.reviewOfTheHead(ctx, "t1", "h2", dropped); ok {
		t.Error("a review the check-in dropped a finding of was reused")
	}
	if _, ok := g.r.reviewOfTheHead(ctx, "t1", "h2", nil); ok {
		t.Error("a first review was reused")
	}
}

func (h heldSignals) Terminate(context.Context, string, string, string) error { return nil }

func (h heldSignals) Alive(context.Context, string) (bool, error)       { return true, nil }
func (h heldSignals) RunningRuns(context.Context) ([]RunningRun, error) { return nil, nil }

func (d downSignals) Terminate(context.Context, string, string, string) error { return nil }

func (d downSignals) Alive(context.Context, string) (bool, error)       { return true, nil }
func (d downSignals) RunningRuns(context.Context) ([]RunningRun, error) { return nil, nil }
