package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	c.ids = append(c.ids, taskID)
	c.mu.Unlock()
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
	if err := g.s.DrawCard(context.Background(), "t1", CaseDraw{Rung: rung, Extend: d, PolicyCeiling: CardCeiling(v.Document, project),
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

	refusal := fmt.Sprintf("the card's draw bound leaves 0 sessions, %s of agent time and 4.00 EUR: rung 2 or the human may draw more", minutesWord(policy.SizeAgentTime("medium", time.Hour)))
	steps := g.steps(t, out.CaseID)
	if len(steps) != 3 || steps[1].Rung != 1 || steps[1].Outcome != StepHandedUp || !strings.Contains(steps[1].Found, "rung 1's answer extend was refused: "+refusal) ||
		steps[1].Decision != "successor" || steps[1].Grounds != "one more session would do" || steps[2].Rung != 3 {
		t.Fatalf("steps = %+v, want rung 1's step handed up naming %q, then the human's", steps, refusal)
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

	steps := g.steps(t, out.CaseID)
	if len(steps) != 3 || steps[1].Rung != 1 || steps[1].Outcome != StepHandedUp || steps[1].Decision != "close" || steps[1].Grounds != "the card's work is done on main" {
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
	steps := bad.steps(t, out.CaseID)
	if len(steps) != 3 || steps[1].Outcome != StepHandedUp || !strings.Contains(steps[1].Found, "the rung's answers were refused twice") {
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
	g.chat.answers = []CheckinResponse{g.handUp(t)}
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
	if len(steps) != 3 || steps[1].Outcome != StepHandedUp || !strings.HasSuffix(steps[1].Found, "the rung did not answer within 35m") || steps[2].Rung != 3 {
		t.Fatalf("steps = %+v, want rung 1's step handed up as unanswered within 35m, then the human's", steps)
	}
	if len(g.forge.comments) != 1 || len(g.asks(t)) != 1 || caseByID(t, g.s, out.CaseID).Status != CaseAtHuman {
		t.Fatalf("comments %d, asks %d, want the one face", len(g.forge.comments), len(g.asks(t)))
	}
}

// A moment's own human answer is rung 1's step: the case faces the human
// with no conversation, and the step is the moment's record. A goal
// conflict is the human's alone, and so is any other reason the moment
// gave, since rung 1 never judges one stop twice.
func TestRung1MomentHumanIsItsStep(t *testing.T) {
	for _, kind := range []string{CheckGoalConflict, CheckSpend} {
		g := newRungRig(t, "success")
		out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: "the check-in sends this to a human: " + kind, Kind: CaseCheckinHuman,
			StateKey: "k-" + kind, Extra: map[string]any{"reason_kind": kind}}, tripAttempt{N: 1})
		if err != nil {
			t.Fatal(err)
		}
		g.settle()
		steps := g.steps(t, out.CaseID)
		if out.Rung != 3 || g.chat.calls != 0 || len(steps) != 3 || steps[1].Rung != 1 || steps[1].Actor != CheckinDecider || steps[1].Outcome != StepHandedUp ||
			steps[1].Decision != CheckHuman || !strings.Contains(steps[1].Grounds, kind) || steps[2].Rung != 3 {
			t.Fatalf("%s: outcome %+v, %d conversations, steps %+v; want the face at once and the moment's record as rung 1's step", kind, out, g.chat.calls, steps)
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

// With no check-in alias, a card of no class, the trip behaves as before:
// the face at once, no step of rung 1.
func TestRung1NoAliasFacesTheHumanAtOnce(t *testing.T) {
	g := newRungRig(t, "success")
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, tripCap)
	g.settle()
	if out.Rung != 3 || g.chat.calls != 0 || out.Comment == 0 || slices.ContainsFunc(g.steps(t, out.CaseID), func(s CaseStep) bool { return s.Rung == 1 }) {
		t.Fatalf("outcome %+v, %d conversations; want the face at once and no step of rung 1", out, g.chat.calls)
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
	steps := g.steps(t, out.CaseID)
	faces := strings.Join(g.forge.comments, "\n")
	asks := g.asks(t)
	blob, _ := json.Marshal([]any{steps, asks, faces})
	for _, secret := range []string{"user:pass", "abc123def456ghi789"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("%q reached the record: %s", secret, blob)
		}
	}
	if len(steps) != 3 || len(steps[1].Read) != 1 || steps[1].Read[0].Tool != "read_file" {
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
	last := g2.steps(t, c.ID)
	if got := last[len(last)-1].Found; strings.Contains(got, "user:pass") || strings.Contains(got, "abc123def456ghi789") || !strings.Contains(got, "could not run") {
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
