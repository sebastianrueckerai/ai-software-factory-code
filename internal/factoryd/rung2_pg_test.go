package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// rungTwoRig is a rung rig whose console wakes rung 2, on a manual project
// whose member sebastian the Waker wakes, under judgment.
func rungTwoRig(t *testing.T, judgment string) (rungRig, *Waker, *fakeWakeStarter) {
	t.Helper()
	g := newRungRig(t, "success")
	g.r.RungTwo = true
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
	if err != nil || !handled {
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
	before := len(starter.recorded())
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if got := len(starter.recorded()); got != before {
		t.Fatalf("%d wakes after the face, want %d: the face of a case rung 2 worked wakes nobody", got, before)
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
// as unanswered within 35m.
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
	if len(g.forge.comments) != 1 || len(asks) != 1 || caseByID(t, g.s, out.CaseID).Status != CaseAtHuman ||
		!strings.Contains(asks[0].Detail["info"].(string), "The factory agent found: the rung did not answer within 35m") {
		t.Fatalf("comments %d, asks %+v; want the one face", len(g.forge.comments), asks)
	}
}

// A spent judgment budget leaves nothing for rung 2's wake: the case goes to
// the human unjudged, as rung 1's does.
func TestRung2NeedsTheJudgmentBudget(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	task, err := g.r.judgmentTask(context.Background(), Task{ID: "t1", Repository: "demo-app/demo-app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET spent_eur = budget_eur + 1 WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, tripCap)
	if out.Rung != 3 || len(g.rows(t, EventCaseClimbed)) != 0 {
		t.Fatalf("outcome %+v; want the face at once with the judgment budget spent", out)
	}
}

// An analysis and a recommendation reach the face and the step masked: URL
// userinfo and a bearer value.
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

// The wake text's statement and board digest are masked, and a statement
// that cannot be built is named, its error masked.
func TestWakeTextMasksWhatItCannotRead(t *testing.T) {
	const url, bearer = "https://user:pass@host/x", "Authorization: Bearer abc123def456ghi789"
	w, s, starter, _ := newWakeRig(t, nil)
	w.Statement = func(context.Context, int64) (string, error) {
		return "", (&Runner{}).scrubbed(errString("read " + url + " " + bearer))
	}
	w.Board = func(context.Context, string) (string, error) {
		return "", (&Runner{}).scrubbed(errString("board " + url + " " + bearer))
	}
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40, Kind: EventCaseClimbed, Via: ViaFactoryd,
		Detail: map[string]any{"case_id": int64(7), "rung": 2, "kind": "session_cap", "problem": "the session cap"}}); err != nil {
		t.Fatal(err)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	calls := starter.recorded()
	if len(calls) != 1 || calls[0].birth != "case_climbed" || strings.Contains(calls[0].text, "user:pass") || strings.Contains(calls[0].text, "abc123def456ghi789") ||
		!strings.Contains(calls[0].text, "The statement of case 7 could not be built") || !strings.Contains(calls[0].text, "Wake: 1 case waits for the factory agent.") {
		t.Fatalf("wakes = %+v, want one masked wake that names the statement it could not build", calls)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

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
