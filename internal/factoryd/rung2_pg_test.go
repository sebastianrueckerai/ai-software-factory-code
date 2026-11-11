package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// rungTwoRig is a rung rig whose console wakes rung 2, on a manual project
// whose member sebastian the Waker wakes, under judgment.
func rungTwoRig(t *testing.T, judgment string) (rungRig, *Waker, *fakeWakeStarter) {
	t.Helper()
	g := newRungRig(t, "success")
	for _, table := range []string{"chat_messages", "chat_conversations", "chat_wake_cursor", "chat_wake_state"} {
		if _, err := g.s.db.ExecContext(context.Background(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	v := g.r.cfg.Policy()
	p := v.Projects["demo-app"]
	p.Autonomy = policy.AutonomyManual
	p.Notify = []string{"sebastian"}
	p.Escalation = &policy.Escalation{Judgment: judgment}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.cfg.Policy = func() policy.Version { return v }
	starter := &fakeWakeStarter{}
	w := &Waker{Store: g.s, Start: starter, Now: g.r.clock, Current: g.r.cfg.Policy,
		Statement: g.r.RungTwoStatement, Board: g.r.BoardText}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("the seeding pass: %v", err)
	}
	return g, w, starter
}

func (g rungRig) stepsOfRung(t *testing.T, id int64, rung int) []CaseStep {
	t.Helper()
	return slices.DeleteFunc(g.steps(t, id), func(s CaseStep) bool { return s.Rung != rung })
}

// A rung 1 hand-up on a manual project under judgment always goes to rung 2
// and not to the human. By hand: no issue comment is posted, one
// case_climbed row names the case and kind, the case's steps are rung 0's
// (handed up), rung 1's (handed up) and rung 2's in progress, the case is
// open and owned by rung 2. The Waker's pass starts one wake turn on the
// row, birth case_climbed, whose text holds the case's problem, the board
// digest's head line and close among rung 2's actions; the turn's answer on
// rung 2 resolves the case with rung 2's own step and no second one.
func TestRung1HandUpGoesToRung2WhichAnswers(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	ctx := context.Background()

	if len(g.forge.comments) != 0 || len(g.asks(t)) != 0 {
		t.Fatalf("comments %q, asks %d; want no face before rung 2 answers", g.forge.comments, len(g.asks(t)))
	}
	rows := g.rows(t, EventCaseClimbed)
	if len(rows) != 1 || rows[0].Detail["case_id"] != float64(out.CaseID) || rows[0].Detail["rung"] != float64(2) || rows[0].Detail["kind"] != string(CaseSessionCap) {
		t.Fatalf("case_climbed rows %+v, want one for case %d at rung 2 of kind session_cap", rows, out.CaseID)
	}
	steps := g.steps(t, out.CaseID)
	if len(steps) != 3 || steps[0].Rung != 0 || steps[1].Rung != 1 || steps[1].Outcome != StepHandedUp ||
		steps[2].Rung != 2 || steps[2].Outcome != StepInProgress || steps[2].Actor != ChatUser || steps[2].DueAt == nil {
		t.Fatalf("steps = %+v, want rung 0 and 1 handed up and rung 2 in progress", steps)
	}
	c := caseByID(t, g.s, out.CaseID)
	if c.Status != CaseOpen || c.OwnerRung != 2 {
		t.Fatalf("case = %+v, want open and owned by rung 2", c)
	}

	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != 1 || calls[0].birth != "case_climbed" {
		t.Fatalf("wakes = %+v, want one case_climbed turn", calls)
	}
	for _, want := range []string{c.Problem, "\ncards 1 live · slots ", "\n- close", "End case "} {
		if !strings.Contains(calls[0].text, want) {
			t.Fatalf("the wake text %q misses %q", calls[0].text, want)
		}
	}

	g.atAutonomy(policy.AutonomyAutonomous) // at manual the extend is a proposal
	reads := []CaseRead{{Tool: "card_facts", Bytes: 10}}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2,
		Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "one more session"}, Read: reads}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	c = caseByID(t, g.s, out.CaseID)
	own := g.stepsOfRung(t, out.CaseID, 2)
	if c.Status != CaseResolved || c.Resolution.Rung != 2 || len(own) != 1 || own[0].Outcome != StepResolved || own[0].Decision != ActExtend ||
		!strings.HasSuffix(own[0].Grounds, "one more session") || len(own[0].Read) != 1 || own[0].Read[0].Tool != "card_facts" {
		t.Fatalf("case %+v, rung 2 steps %+v; want it resolved on rung 2 by its one step with the turn's reads and grounds", c, own)
	}
}

// Rung 2's analysis with a recommended close faces the human once with the
// recommendation as "close: <grounds> (the factory agent)", the case at the
// human, rung 2's step handed up with the analysis as its finding, the
// action as its decision and the reads; the face's question_asked wakes
// nobody.
func TestRung2AnalysisFacesTheHumanWithItsRecommendation(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	reads := []CaseRead{{Tool: "card_facts", Bytes: 10}}
	handled, err := g.r.HandUpRung2(context.Background(), "t1", out.CaseID, "the pull request is merged on main",
		RungTwoRecommendation{Action: ActClose, Grounds: "nothing is left to build"}, reads)
	if err != nil || handled != out.CaseID {
		t.Fatalf("HandUpRung2 = %v, %v; want handled", handled, err)
	}
	asks := g.asks(t)
	if len(g.forge.comments) != 1 || len(asks) != 1 || asks[0].Detail["recommendation"] != "close: nothing is left to build (the factory agent)" ||
		!strings.Contains(asks[0].Detail["info"].(string), "The factory agent found: the pull request is merged on main") {
		t.Fatalf("comments %q, asks %+v; want one face recommending close with the grounds", g.forge.comments, asks)
	}
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman || c.OwnerRung != 3 {
		t.Fatalf("case = %+v, want at the human", c)
	}
	own := g.stepsOfRung(t, out.CaseID, 2)
	if len(own) != 1 || own[0].Outcome != StepHandedUp || own[0].Decision != ActClose || own[0].Grounds != "nothing is left to build" ||
		own[0].Found != "the pull request is merged on main" || len(own[0].Read) != 1 {
		t.Fatalf("rung 2 steps = %+v, want one handed up with found, decision, grounds and reads", own)
	}
	if faced := g.signalsOf(SignalCaseFaced); len(faced) != 1 || faced[0].sig.Case.Comment == 0 {
		t.Fatalf("case_faced signals = %+v, want one carrying the face's comment", faced)
	}
	// a pass past the gap between wakes: the face wakes nobody, and an
	// unrelated ask of the same pass still does
	g.r.now = func() time.Time { return time.Now().Add(time.Minute) }
	before := len(starter.recorded())
	wakeAsk(t, g.s, "demo-app/demo-app", "t-x", "Is the other card fine?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != before+1 || !strings.Contains(calls[before].text, "Is the other card fine?") || strings.Contains(calls[before].text, "Grant it one more session") {
		t.Fatalf("wakes = %+v; want one more, for the unrelated ask alone: the face of a case rung 2 worked wakes nobody", calls)
	}
}

// A recommendation of an action no human takes is refused with the actions
// that fit, and the step stays in progress.
func TestRung2AnalysisRefusesAnUnknownAction(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	_, err := g.r.HandUpRung2(context.Background(), "t1", out.CaseID, "found", RungTwoRecommendation{Action: ActWithdraw, Grounds: "x"}, nil)
	var refused ErrActRefused
	if !asRefused(err, &refused) || !strings.Contains(err.Error(), "withdraw") {
		t.Fatalf("err = %v, want a refusal naming withdraw", err)
	}
	if own := g.stepsOfRung(t, out.CaseID, 2); len(own) != 1 || own[0].Outcome != StepInProgress {
		t.Fatalf("rung 2 steps = %+v, want the step in progress", own)
	}
}

func asRefused(err error, target *ErrActRefused) bool {
	r, ok := err.(ErrActRefused)
	if ok {
		*target = r
	}
	return ok
}

// With judgment autonomous_and_above on a manual project nothing judges: the
// face at once, no case_climbed, no step of rung 2.
func TestRung2DoesNotWakeWhereTheRungsDoNotJudge(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAutonomousAndAbove)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	if out.Rung != 3 || len(g.forge.comments) != 1 || len(g.rows(t, EventCaseClimbed)) != 0 || len(g.stepsOfRung(t, out.CaseID, 2)) != 0 {
		t.Fatalf("outcome %+v, comments %d; want the face at once and no rung 2", out, len(g.forge.comments))
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("wakes = %+v, want none: the rungs do not judge here", calls)
	}
}

// A card with no check-in alias goes from rung 0 to rung 2, and a monitor
// flag on it posts no comment on the issue until rung 2 handed it up.
func TestMonitorFlagGoesToRung2WithoutAComment(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: monitorFlagPrefix + "answer_changed_nothing: the extend bought nothing",
		Head: "h2", Kind: CaseMonitorFlag, StateKey: "answer_changed_nothing:h2"}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rung != 2 || len(g.forge.comments) != 0 || len(g.rows(t, EventCaseClimbed)) != 1 || len(g.stepsOfRung(t, out.CaseID, 2)) != 1 {
		t.Fatalf("outcome %+v, comments %q; want the flag with rung 2 and no comment on the issue", out, g.forge.comments)
	}
	again, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: monitorFlagPrefix + "answer_changed_nothing: the extend bought nothing",
		Head: "h2", Kind: CaseMonitorFlag, StateKey: "answer_changed_nothing:h2"}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if again.Rung != 2 || len(g.stepsOfRung(t, out.CaseID, 2)) != 1 || len(g.rows(t, EventCaseClimbed)) != 1 {
		t.Fatalf("repeat %+v; want the same stop to find rung 2 at work and start no second step", again)
	}
}

// A rung 2 step past its due climbs once: one face, rung 2's step handed up
// as unanswered within 35m, and the face carries what rung 1 recommended and
// found, since rung 2 gave nothing.
func TestRung2ExpiryFacesTheHumanOnce(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 2`, out.CaseID); err != nil {
		t.Fatal(err)
	}
	own := g.stepsOfRung(t, out.CaseID, 2)
	g.r.now = func() time.Time { return own[0].DueAt.Add(time.Minute) }
	for range 2 {
		expired, err := g.s.ExpireSteps(ctx, g.r.clock())
		if err != nil {
			t.Fatal(err)
		}
		if err := g.r.climbExpired(ctx, expired); err != nil {
			t.Fatal(err)
		}
	}
	own = g.stepsOfRung(t, out.CaseID, 2)
	if len(own) != 1 || own[0].Outcome != StepHandedUp || !strings.HasSuffix(own[0].Found, "the rung did not answer within 35m") {
		t.Fatalf("rung 2 steps = %+v, want one handed up as unanswered within 35m", own)
	}
	asks := g.asks(t)
	if len(g.forge.comments) != 1 || len(asks) != 1 || caseByID(t, g.s, out.CaseID).Status != CaseAtHuman {
		t.Fatalf("comments %d, asks %+v; want the one face", len(g.forge.comments), asks)
	}
	info, rec := asks[0].Detail["info"].(string), asks[0].Detail["recommendation"]
	if !strings.Contains(info, "The factory agent did not answer within 35m.") || strings.Contains(info, "The factory agent found") ||
		!strings.Contains(info, "The check-in found: ") || rec != "The check-in recommends close: the card's work is done on main" {
		t.Fatalf("info %q, recommendation %v; want the expiry in words, rung 1's finding and its recommendation", info, rec)
	}
}

// A spent judgment budget goes through rung 1's spent path in full on the
// rung 2 path: the case faces the human with the mark on its problem, rung
// 0's step naming the chat task's money, and the one judgment_budget case,
// and no rung 2 starts. A budget with less left than one call needs counts as
// spent too: 0.01 EUR left of the 0.01536 EUR a call on big needs is spent,
// 0.02 EUR is not.
func TestRung2NeedsTheJudgmentBudget(t *testing.T) {
	for _, left := range []float64{0, 0.01, 0.02} {
		g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
		g.r.store = projectStore{g.s}
		cardForge, _ := newAlertForgejo(t)
		g.r.ChatBudget = &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: cardForge, Timeline: g.r.timeline}
		ctx := context.Background()
		if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
			t.Fatal(err)
		}
		chat := g.spendChat(t)
		if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur - $2 WHERE id = $1`, chat.ID, left); err != nil {
			t.Fatal(err)
		}
		out := g.trip(t, tripCap)
		c := caseByID(t, g.s, out.CaseID)
		if left >= 0.02 {
			if out.Rung != 2 || len(g.rows(t, EventCaseClimbed)) != 1 {
				t.Fatalf("left %v: outcome %+v; want rung 2 to start", left, out)
			}
			continue
		}
		var cases int
		if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM cases WHERE kind = 'judgment_budget'`).Scan(&cases); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("not judged, judgment budget spent: the chat task %s has %.2f of 10.00 EUR left", chat.ID, left)
		if out.Rung != 3 || len(g.rows(t, EventCaseClimbed)) != 0 || !strings.HasSuffix(c.Problem, unjudgedSuffix) || cases != 1 ||
			!slices.ContainsFunc(g.steps(t, c.ID), func(s CaseStep) bool { return s.Rung == 0 && s.Found == want }) {
			t.Fatalf("left %v: outcome %+v, problem %q, %d money cases, steps %+v; want the spent path in full", left, out, c.Problem, cases, g.steps(t, c.ID))
		}
	}
}

func TestRung2AnalysisIsMasked(t *testing.T) {
	const url, bearer = "https://user:pass@host/x", "Authorization: Bearer abc123def456ghi789"
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	if _, err := g.r.HandUpRung2(context.Background(), "t1", out.CaseID, "read "+url+" "+bearer,
		RungTwoRecommendation{Action: ActClose, Grounds: "see " + url + " with " + bearer}, nil); err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal([]any{g.steps(t, out.CaseID), g.asks(t), g.forge.comments})
	for _, secret := range []string{"user:pass", "abc123def456ghi789"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("%q reached the face or the step: %s", secret, blob)
		}
	}
	if !strings.Contains(string(blob), "close: ") {
		t.Fatalf("the face lost its recommendation: %s", blob)
	}
}

// brokenReads is a store whose case and board reads fail with an error that
// carries URL userinfo and a bearer value.
type brokenReads struct{ *PGStore }

const (
	secretURL    = "https://user:pass@host/x"
	secretBearer = "Authorization: Bearer abc123def456ghi789"
)

func (brokenReads) CaseByID(context.Context, int64) (Case, error) {
	return Case{}, fmt.Errorf("read the case: GET %s: %s", secretURL, secretBearer)
}

func (brokenReads) BoardTasks(context.Context, string) ([]BoardTask, error) {
	return nil, fmt.Errorf("read the board: GET %s: %s", secretURL, secretBearer)
}

func (g rungRig) climbedRow(t *testing.T, id int64) {
	t.Helper()
	c, err := g.s.CaseByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Record(context.Background(), Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventCaseClimbed, Via: ViaFactoryd,
		Detail: map[string]any{"case_id": id, "rung": 2, "kind": "other", "problem": textutil.Truncate(g.r.scrub(c.Problem), 300)}}); err != nil {
		t.Fatal(err)
	}
}

// The wake text goes through the real statement and board reads, masked: a
// secret in a case's problem and in its card reaches the text masked, and
// the errors of both reads, which carry a secret, are masked and named.
func TestWakeTextIsMaskedThroughTheRealReads(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseOpen,
		Problem: "stuck at " + secretURL + " with " + secretBearer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepInProgress}); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateTask(ctx, Task{ID: "t3", Kind: "agent", Status: "running", Objective: "z", Repository: "demo-app/demo-app", IssueNumber: 14, Track: policy.TrackPlan, Size: "small", BudgetEUR: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET plan_name = $1 WHERE id = 't3'`, "plan "+secretURL+" "+secretBearer); err != nil {
		t.Fatal(err)
	}
	g.climbedRow(t, c.ID)
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "plans:") || !strings.Contains(calls[0].text, "plan https://[redacted]@host/x") || !strings.Contains(calls[0].text, "stuck at https://[redacted]@host/x") || !strings.Contains(calls[0].text, "cards 2 live") ||
		strings.Contains(calls[0].text, "user:pass") || strings.Contains(calls[0].text, "abc123def456ghi789") {
		t.Fatalf("wakes = %+v, want one masked wake with the statement and the board", calls)
	}
	if n := strings.Count(calls[0].text, "stuck at"); n != 1 {
		t.Fatalf("the problem stands %d times in %q, want once", n, calls[0].text)
	}

	second, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k2", Status: CaseOpen, Problem: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: second.ID, Rung: 2, Actor: ChatUser, Outcome: StepInProgress}); err != nil {
		t.Fatal(err)
	}
	g.r.store = brokenReads{g.s}
	g.r.now = func() time.Time { return time.Now().Add(time.Minute) }
	g.climbedRow(t, second.ID)
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls = starter.recorded()
	text := calls[len(calls)-1].text
	if len(calls) != 2 || strings.Contains(text, "user:pass") || strings.Contains(text, "abc123def456ghi789") ||
		!strings.Contains(text, "could not be built: read the case: GET https://[redacted]@host/x") || !strings.Contains(text, "The board digest could not be built: read the board") {
		t.Fatalf("wakes = %+v, want the failed reads named and masked", calls)
	}
}

// Register entry 7: at an agent-time cap rung 2's first action is the extend
// that raises agent time, a replan is offered and not refused, and a probe
// that buys sessions alone is not what the extend lists.
func TestRung2OffersTheExtendFirstAtAnAgentTimeCap(t *testing.T) {
	f := CaseFacts{Kind: CaseAgentTimeCap, PROpen: true, Head: "h2", Checks: "green", ReviewApproved: true, FloorFree: true,
		SessionCap: 3, Attempts: 1, AgentTimeS: 1200, AgentTimeCapS: 1200, SessionLeastS: 300,
		PoolLeft: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour, EUR: 20}}
	var names []string
	for _, a := range Offered(f, 2) {
		names = append(names, a.Name)
	}
	if len(names) == 0 || names[0] != ActExtend || !slices.Contains(names, ActReplan) {
		t.Fatalf("rung 2 is offered %v, want extend first and replan among them", names)
	}
	if why := (&Runner{}).rungStatement(Case{Kind: CaseAgentTimeCap, Problem: "agent time"}, f, "digest", nil, 2); !strings.Contains(why, "\n- extend\n- ") ||
		strings.Index(why, "- extend") > strings.Index(why, "- reverify") {
		t.Fatalf("the statement lists %q, want extend ahead of reverify", why)
	}
}

// Register entry 50, first half: a planner that stopped for time is extended
// by rung 2 with the cut as its guidance: the extend is offered first, the
// draw of agent time is taken, and the session's note carries the cut.
func TestRung2ExtendsAPlannerWithTheCutAsGuidance(t *testing.T) {
	const cut = "land one card compiling, then one card per proof"
	f := CaseFacts{Kind: CaseAgentTimeCap, Planner: true, FloorFree: true, SessionCap: 3, Attempts: 1,
		AgentTimeS: 1200, AgentTimeCapS: 1200, SessionLeastS: 300, PoolLeft: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour}}
	offered := Offered(f, 2)
	if len(offered) == 0 || offered[0].Name != ActExtend {
		t.Fatalf("rung 2 is offered %v, want extend first", offered)
	}
	extend, _ := ActionByName(ActExtend)
	res := Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 1800}, Note: cut}
	if why := extend.refusal(f, 2, res); why != "" {
		t.Fatalf("rung 2's extend of a planner with the cut is refused: %s", why)
	}
	if note := (&Runner{}).sessionNote(f, ActInput{Res: res}); !strings.Contains(note, cut) {
		t.Fatalf("the session's note %q lacks the cut", note)
	}
}

// From Task 2.15: rung 2's resume check weighs a cap that binds before the
// card's budget. A day cap reached and the budget short: resume is not
// offered as if a session could start now, and its refusal names the cap.
func TestRung2DoesNotOfferResumeWhileACapBinds(t *testing.T) {
	until := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	line, _, _ := strings.Cut(capWaitReason(policy.RefusedDayCap, "the gateway", "the day's cap", until), "\n")
	f := CaseFacts{Kind: CaseSessionCap, FloorEUR: 1, LeftEUR: 0.2, SessionCap: 3, Attempts: 1, AgentTimeS: 100, AgentTimeCapS: 1200, SessionLeastS: 300,
		CapWait: line, PoolLeft: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour}}
	resume, _ := ActionByName(ActResume)
	why := resume.refusal(f, 2, Resolution{Action: ActResume})
	if why == "" || !strings.Contains(why, "a cap binds before a session can start: "+line) {
		t.Fatalf("resume refusal = %q, want the day cap's line", why)
	}
	for _, a := range Offered(f, 2) {
		if a.Name == ActResume || a.Name == ActContinue || a.Name == ActHandover {
			t.Fatalf("rung 2 is offered %s while the day cap binds", a.Name)
		}
	}
	f.CapWait = ""
	if why := resume.refusal(f, 2, Resolution{Action: ActResume}); strings.Contains(why, "a cap binds") {
		t.Fatalf("resume refusal = %q without a cap wait", why)
	}
}

// policyRungsOff is v with every project at manual under escalation.judgment
// autonomous_and_above: no rung judges, so a stop faces the human at once.
func policyRungsOff(v policy.Version) policy.Version {
	projects := maps.Clone(v.Projects)
	for name, p := range projects {
		p.Autonomy = policy.AutonomyManual
		p.Escalation = judgesAtAutonomous
		projects[name] = p
	}
	v.Projects = projects
	return v
}

func rungsOff(r *Runner) {
	v := policyRungsOff(r.cfg.Policy())
	r.cfg.Policy = func() policy.Version { return v }
}

func (g monitorRig) rungsOff() {
	rungsOff(g.m.Runner)
	g.m.Policy = func() policy.Tree { return g.m.Runner.cfg.Policy().Tree }
}

// afterRung2 lets rung 2's step on case id run past its bound, as the sweep
// finds it: the step ends handed up and the case climbs to the human.
func (g rungRig) afterRung2(t *testing.T, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 2`, id); err != nil {
		t.Fatal(err)
	}
	own := g.stepsOfRung(t, id, 2)
	if len(own) != 1 || own[0].DueAt == nil {
		t.Fatalf("rung 2 steps = %+v, want one with a bound", own)
	}
	g.r.now = func() time.Time { return own[0].DueAt.Add(time.Minute) }
	expired, err := g.s.ExpireSteps(ctx, g.r.clock())
	if err != nil {
		t.Fatal(err)
	}
	if err := g.r.climbExpired(ctx, expired); err != nil {
		t.Fatal(err)
	}
	g.settle()
}

// A wake's case_climbed row whose step ended since wakes nobody, and an
// ended step's face is no wake either; a live step's row wakes.
func TestStaleCaseClimbedWakesNobody(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	g.r.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("wakes = %+v, want none: the step ended before the pass, and the face worked by rung 2 wakes nobody", calls)
	}
}

// The recommended args are masked on the step: nested strings too.
func TestRung2RecommendedArgsAreMasked(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	if _, err := g.r.HandUpRung2(context.Background(), "t1", out.CaseID, "found",
		RungTwoRecommendation{Action: ActClose, Grounds: "x", Args: map[string]any{"why": "see " + secretURL, "nested": map[string]any{"h": secretBearer}, "n": 3}}, nil); err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(g.stepsOfRung(t, out.CaseID, 2))
	if strings.Contains(string(blob), "user:pass") || strings.Contains(string(blob), "abc123def456ghi789") || !strings.Contains(string(blob), "[redacted]@host") {
		t.Fatalf("the recommended args reached the step as %s, want them masked", blob)
	}
}

// A case id of another card is refused with the card's open cases, and the
// analysis does not land in the card's ask unseen.
func TestHandUpRung2RefusesAnotherCardsCase(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "running", Objective: "y", Repository: "demo-app/demo-app", Track: policy.TrackDirect, Size: "medium", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	other, _, err := g.s.OpenCase(ctx, Case{TaskID: "t2", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "p"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.r.HandUpRung2(ctx, "t1", other.ID, "found", RungTwoRecommendation{Action: ActClose}, nil)
	want := fmt.Sprintf("case %d is not a case of this card: its open cases are %d", other.ID, out.CaseID)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// An action table whose fitting action is missing lists the table as it is.
func TestOrderedActionsWithoutTheFittingAction(t *testing.T) {
	saved := Actions
	t.Cleanup(func() { Actions = saved })
	Actions = slices.DeleteFunc(slices.Clone(saved), func(a Action) bool { return a.Name == ActExtend })
	if got := orderedActions(CaseFacts{Kind: CaseAgentTimeCap}); len(got) != len(Actions) {
		t.Fatalf("%d actions, want the table's %d", len(got), len(Actions))
	}
}

// The card digest renders a supervisor's message beside its check-in line,
// masked and cut to 400 runes, so rung 1 and rung 2 read the cut a
// supervisor wrote; rung 2's wake text carries it through the statement.
// This is register entry 50's pin: the cut is on the card before rung 2 acts.
func TestRung2ReadsTheSupervisorsCutOnTheCard(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	const cut = "land one card that compiles, then one card per proof"
	long := strings.Repeat("é", 450)
	ctx := context.Background()
	for _, guidance := range []string{cut + " " + secretURL, long} {
		if err := g.r.timeline.Append(ctx, "t1", "checkin", map[string]any{"decision": "message", "moment": "stalled", "guidance": guidance}); err != nil {
			t.Fatal(err)
		}
	}
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	g.trip(t, tripCap)
	g.settle()
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "message: "+cut+" https://[redacted]@host/x") || strings.Contains(calls[0].text, "user:pass") {
		t.Fatalf("wakes = %+v, want the supervisor's message, masked, in rung 2's wake text", calls)
	}
	if !strings.Contains(calls[0].text, "message: "+strings.Repeat("é", 400)+"…\n") || strings.Contains(calls[0].text, strings.Repeat("é", 401)) {
		t.Fatalf("the long message is not cut to 400 runes in %q", calls[0].text)
	}
}

// The turn's spend lands on rung 2's step: AddRungCost adds to the case's
// newest step of the rung.
func TestAddRungCostLandsOnTheNewestStepOfTheRung(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	ctx := context.Background()
	for _, eur := range []float64{0.25, 0.5} {
		if err := g.s.AddRungCost(ctx, out.CaseID, 2, eur); err != nil {
			t.Fatal(err)
		}
	}
	if own := g.stepsOfRung(t, out.CaseID, 2); len(own) != 1 || own[0].CostEUR != 0.75 {
		t.Fatalf("rung 2 steps = %+v, want a cost of 0.75", own)
	}
	if err := g.s.AddRungCost(ctx, out.CaseID, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := g.s.AddRungCost(ctx, 999999, 2, 1); err == nil {
		t.Fatal("a cost on a case with no step was accepted")
	}
}

// Register entry 50's shape on two cards: child #1587 of plan #1547 stopped
// on its size bound and the check-in decided a replan whose reason runs to 354
// runes and whose guidance names the cut; the plan's replan step is written by
// the function ReplanChild writes it with. The plan's planner then stopped for
// time. Rung 2's wake text for the plan's case holds the guidance, masked and
// cut to 400 runes, beside the digest's replan line, and the wake names the
// climbed case for the turn's cost.
func TestRung2ReadsTheCutAChildsReplanCarriedToItsPlan(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET track = 'plan', issue_number = 1547 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateTask(ctx, Task{ID: "c1587", Kind: "agent", Status: "running", Objective: "the size bound", Repository: "demo-app/demo-app", IssueNumber: 1587,
		Track: policy.TrackDirect, Source: SourcePlan, Parent: "t1", PlanKey: "api", Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	const cut = "land one card that compiles, then one card per proof"
	work := replanText(&Trip{Kind: TripSize, Detail: "1774 lines against 1742"},
		Decision{Action: ActionReplan, Reason: strings.Repeat("r", 354), Guidance: cut + " " + secretURL + " " + strings.Repeat("é", 450)})
	if _, err := g.r.timeline.AppendUnlessPending(ctx, "t1", stepReplan, cutReplanDetail("c1587", work, 1), replanClosers...); err != nil {
		t.Fatal(err)
	}
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, timeCapReached+"\nlast: no session produced a result")
	g.settle()
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "message: "+cut+" https://[redacted]@host/x") || strings.Contains(calls[0].text, "user:pass") ||
		strings.Contains(calls[0].text, strings.Repeat("é", 401)) || strings.Contains(calls[0].text, "rrrrrrrrrr") || !strings.Contains(calls[0].text, "replan") {
		t.Fatalf("wakes = %+v, want rung 2's wake text to hold the child's guidance, masked and cut", calls)
	}
	if !slices.Equal(calls[0].cases, []int64{out.CaseID}) {
		t.Fatalf("the wake named cases %v, want [%d]", calls[0].cases, out.CaseID)
	}
	d, err := g.r.CardDigest(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Render(), "user:pass") || !strings.Contains(d.Render(), "[redacted]@host") {
		t.Fatalf("the digest's own render holds the secret or lacks the cut: %s", d.Render())
	}
}

// A wake names the cases handed to rung 2 only: the face of a human card in
// the same batch has no rung 2 step, and the turn's cost has none to land on.
func TestWakeNamesOnlyTheClimbedCases(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	other, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: "h", Status: CaseAtHuman, Problem: "a human card"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventQuestionAsked, Via: ViaFactoryd,
		Detail: map[string]any{"ask": "a human card", "case_id": other.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != 1 || !slices.Equal(calls[0].cases, []int64{out.CaseID}) || !strings.Contains(calls[0].text, "a human card") {
		t.Fatalf("wakes = %+v, want one wake that names only case %d and still reads the face", calls, out.CaseID)
	}
}

// A rung 1 that ended without judging (no decision: a refused call, a cap, a
// failed run, an expiry) does not make a judged case: at a spent budget the
// case keeps the mark, and its rung 0 step says "not judged".
func TestRung1ThatDidNotJudgeLeavesTheMark(t *testing.T) {
	for _, found := range []string{"rung 1 could not run: the gateway refused the call for money", "the rung did not answer within 35m"} {
		g := newRungRig(t, "success")
		g.r.cfg.Prices = nil
		ctx := context.Background()
		g.spendChat(t)
		c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "stuck"})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if _, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp, Found: found, EndedAt: &now}); err != nil {
			t.Fatal(err)
		}
		task, _, _ := g.r.task(ctx, "t1")
		if _, err := g.r.climb(ctx, task, c, CaseFacts{}, 1, found); err != nil {
			t.Fatal(err)
		}
		if got := caseByID(t, g.s, c.ID); got.Problem != "stuck"+unjudgedSuffix {
			t.Fatalf("%q: problem %q, want the mark", found, got.Problem)
		}
	}
}

// The scrub of a supervisor's message sits in the digest's own render, which
// the card's life page and the monitor's facts show unscrubbed by anything
// else.
func TestDigestRenderMasksASupervisorsMessage(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if err := g.r.timeline.Append(ctx, "t1", "checkin", map[string]any{"decision": "message", "moment": "stalled", "guidance": "go on " + secretURL + " " + secretBearer}); err != nil {
		t.Fatal(err)
	}
	d, err := g.r.CardDigest(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	text := d.Render()
	if !strings.Contains(text, "message: go on https://[redacted]@host/x") || strings.Contains(text, "user:pass") || strings.Contains(text, "abc123def456ghi789") {
		t.Fatalf("render = %s, want the message masked in the digest itself", text)
	}
}

// The spent check on the rung 2 path prices the call on the class the wake
// runs on: with premium priced at 1e-4 EUR per output token a call needs
// 0.1024 EUR for its 1024 tokens, so 0.05 EUR left is spent, where on big
// (0.01536 EUR) it would start.
func TestRung2SpentCheckPricesTheWakesClass(t *testing.T) {
	for _, premium := range []bool{false, true} {
		g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
		g.r.store = projectStore{g.s}
		ctx := context.Background()
		if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
			t.Fatal(err)
		}
		if premium {
			v := g.r.cfg.Policy()
			v.Document.Prices.MaxOutputTokens = map[string]int{policy.Premium: 24576}
			g.r.cfg.Policy = func() policy.Version { return v }
			g.r.cfg.Prices = aliasPrices{"big": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}, "premium": {In: 1e-5, Out: 1e-4, MaxOutput: 24576}}
		}
		chat := g.spendChat(t)
		if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur - 0.05 WHERE id = $1`, chat.ID); err != nil {
			t.Fatal(err)
		}
		out := g.trip(t, tripCap)
		if started := out.Rung == 2; started == premium {
			t.Fatalf("premium priced %v: outcome %+v; want rung 2 to start only on big's floor", premium, out)
		}
	}
}

// A check-in moment's reason survives rung 2's expiry on the face, and a rung
// 1 that did not answer either is named in the expiry's words.
func TestFaceAfterRung2ExpiryKeepsTheMomentsReasonAndRungOnesExpiry(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: "the check-in sends this to a human: " + CheckSpend, Kind: CaseCheckinHuman,
		StateKey: "k", Extra: map[string]any{"reason_kind": CheckSpend}}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.afterRung2(t, out.CaseID)
	if info := g.infoOf(t, out.CaseID); !strings.Contains(info, "The factory agent did not answer within 35m.") || !strings.Contains(info, "The check-in found: the check-in sends this to a human: "+CheckSpend) {
		t.Fatalf("info = %q, want the moment's reason kept", info)
	}

	h, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	h.chat.onCall = func() {}
	h.chat.answers = []CheckinResponse{h.handUp(t)}
	two := h.trip(t, tripCap)
	h.settle()
	ctx := context.Background()
	if _, err := h.s.db.ExecContext(ctx, `UPDATE case_steps SET found = 'the rung did not answer within 35m' WHERE case_id = $1 AND rung = 1`, two.CaseID); err != nil {
		t.Fatal(err)
	}
	h.afterRung2(t, two.CaseID)
	if info := h.infoOf(t, two.CaseID); !strings.Contains(info, "The check-in did not answer within 35m.") || strings.Contains(info, "found: the rung did not answer") {
		t.Fatalf("info = %q, want rung 1's expiry in the same words", info)
	}
}
