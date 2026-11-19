package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

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
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: ChatUser, Via: ViaChat, Rung: 2,
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
	if len(g.forge.comments) != 1 || len(asks) != 1 || asks[0].Detail["recommendation"] != "nothing is left to build" || asks[0].Detail["recommended_by"] != "the factory agent" ||
		!strings.Contains(asks[0].Detail["info"].(string), "The factory agent found:\n\n> the pull request is merged on main") {
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
	// a pass past the gap between wakes: neither the face nor an unrelated
	// ask of the same pass wakes anyone, for a case climbs to rung 2 on its
	// case_climbed alone
	g.r.PgSetNow(func() time.Time { return time.Now().Add(time.Minute) })
	before := len(starter.recorded())
	wakeAsk(t, g.s, "demo-app/demo-app", "t-x", "Is the other card fine?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != before {
		t.Fatalf("wakes = %+v; want none past the %d before: no ask wakes the agent", calls, before)
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

// With judgment autonomous_and_above on an assisted project rung 1 does not
// judge, and rung 2 takes the case all the same: a trip of kind other has
// no rung 1 step, one rung 2 step in progress and one case_climbed row, on
// which the Waker starts one case_climbed turn.
func TestRung2TakesACaseWhereRung1DoesNotJudge(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAutonomousAndAbove)
	g.atAutonomy(policy.AutonomyAssisted)
	out := g.trip(t, "the run stopped on something no rule names")
	g.settle()
	if c := caseByID(t, g.s, out.CaseID); c.Kind != CaseOther || c.Status != CaseOpen || out.Rung != 2 {
		t.Fatalf("outcome %+v, case %s %s; want the other case open with rung 2", out, c.Kind, c.Status)
	}
	if one, two := g.stepsOfRung(t, out.CaseID, 1), g.stepsOfRung(t, out.CaseID, 2); len(one) != 0 || len(two) != 1 || two[0].Outcome != StepInProgress {
		t.Fatalf("rung 1 steps %+v, rung 2 steps %+v; want none and one in progress", one, two)
	}
	if rows := g.rows(t, EventCaseClimbed); len(rows) != 1 || rows[0].Detail["case_id"] != float64(out.CaseID) || len(g.forge.comments) != 0 {
		t.Fatalf("case_climbed rows %+v, comments %d; want one for case %d and no face", rows, len(g.forge.comments), out.CaseID)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 1 || calls[0].birth != EventCaseClimbed || !slices.Equal(calls[0].cases, []int64{out.CaseID}) {
		t.Fatalf("wakes = %+v, want one case_climbed turn on case %d", calls, out.CaseID)
	}
}

// A plan's wait on its failed card sits beside the plan, so its case lives
// on after the plan is done: its rung 2 step past its due ends in the
// case-wake pass, and the case reaches its face, one comment on the
// plan's issue, at the human.
func TestAPlanWaitCaseOnADonePlanReachesItsFace(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET track = 'plan', status = 'done', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	reason := planCardFailedReason(Task{ID: "t-a", IssueNumber: 7, PlanKey: "api"})
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseDependencyFailed, StateKey: "t-a",
		Problem: reason, Facts: map[string]any{"reason": reason, "face": reason}})
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(-time.Minute)
	if _, err := g.s.StartRungStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepInProgress, DueAt: &due}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	v := g.r.PgCfg().Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: time.Now}
	if err := m.WakeCases(ctx); err != nil {
		t.Fatalf("WakeCases: %v", err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("case %s with %d comments, want it at the human with one face", got.Status, len(g.forge.comments))
	}
}

// refusedTrip is a trip on the rung 2 rig whose check-in's one call the
// provider refuses: rung 1's step ends "not judged: the model provider
// refused: provider refused", and rung 2's is in progress.
func refusedTrip(t *testing.T) (rungRig, int64) {
	t.Helper()
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	out := g.trip(t, tripCap)
	g.settle()
	one, two := g.stepsOfRung(t, out.CaseID, 1), g.stepsOfRung(t, out.CaseID, 2)
	if len(one) != 1 || one[0].Found != "not judged: the model provider refused: provider refused" || len(two) != 1 || two[0].Outcome != StepInProgress {
		t.Fatalf("rung 1 steps %+v, rung 2 steps %+v; want rung 1's refused and rung 2 in progress", one, two)
	}
	return g, out.CaseID
}

// refusedSeen is the agent's view of a case whose rungs the provider
// refused, rung 2 last with "the model refused the call (400): bad
// request": one plain sentence, the provider's words left to the steps.
const refusedSeen = "The factory agent did not judge it: the model provider refused. The provider's words are in the card's life."

// Both rungs refused: the provider refuses rung 1's call and rung 2's wake
// turn, whose refusal hands the case up at once. The face comes in that
// call: one comment, the agent's view saying that no rung judged the case
// and what the provider said, the problem and the ask as the case's own,
// rung 2's step ended with the refusal as its finding and rung 0's step
// with the mark.
func TestBothRungsRefusedFaceMarkedNotJudged(t *testing.T) {
	g, id := refusedTrip(t)
	if err := g.r.RefusedRung2(context.Background(), []int64{id}, "the model refused the call (400): bad request"); err != nil {
		t.Fatalf("RefusedRung2: %v", err)
	}
	c := caseByID(t, g.s, id)
	asks := g.asks(t)
	if c.Status != CaseAtHuman || strings.Contains(c.Problem, "not judged") || len(g.forge.comments) != 1 || len(asks) != 1 {
		t.Fatalf("case %s %q, comments %d, asks %d; want one face and the problem as the case's own", c.Status, c.Problem, len(g.forge.comments), len(asks))
	}
	info, _ := asks[0].Detail["info"].(string)
	if ask, _ := asks[0].Detail["ask"].(string); strings.Contains(ask, "not judged") || info != refusedSeen ||
		strings.Contains(info, "400") || strings.Contains(info, "bad request") {
		t.Fatalf("the face's ask %q, info %q; want the ask as the case's own and the view %q, with none of the provider's words", ask, info, refusedSeen)
	}
	if zero := g.stepsOfRung(t, id, 0); !slices.ContainsFunc(zero, func(s CaseStep) bool {
		return s.Found == "not judged: the model provider refused: the model refused the call (400): bad request"
	}) {
		t.Fatalf("rung 0 steps %+v, want one marked not judged with the refusal", zero)
	}
	if two := g.stepsOfRung(t, id, 2); len(two) != 1 || two[0].Outcome != StepHandedUp ||
		two[0].Found != "not judged: the model provider refused: the model refused the call (400): bad request" {
		t.Fatalf("rung 2 steps %+v, want one handed up with the refusal", two)
	}
}

// Rung 1 refused, rung 2 judged: its hand-up faces the human with its
// finding and a line for the rung that could not judge, and no mark.
func TestARung2HandUpAfterARefusedRung1FacesUnmarked(t *testing.T) {
	g, id := refusedTrip(t)
	if _, err := g.r.HandUpRung2(context.Background(), "t1", id, "the card's work is merged on main", RungTwoRecommendation{}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	c := caseByID(t, g.s, id)
	asks := g.asks(t)
	if c.Status != CaseAtHuman || strings.Contains(c.Problem, "(not judged") || len(asks) != 1 {
		t.Fatalf("case %s %q, asks %d; want one face and no mark", c.Status, c.Problem, len(asks))
	}
	info, _ := asks[0].Detail["info"].(string)
	if ask, _ := asks[0].Detail["ask"].(string); strings.Contains(ask, "(not judged") ||
		!strings.Contains(info, "The factory agent found:\n\n> the card's work is merged on main") ||
		!strings.Contains(info, "The check-in did not judge it: the model provider refused.") {
		t.Fatalf("ask %q, info %q; want no mark, rung 2's finding and the check-in's line", ask, info)
	}
}

// A card with no check-in alias goes from rung 0 to rung 2, and a monitor
// flag on it posts no comment on the issue until rung 2 handed it up.
func TestMonitorFlagGoesToRung2WithoutAComment(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	if _, err := g.s.PgDB().ExecContext(context.Background(), `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
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
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE case_steps SET started_at = due_at - interval '35 minutes' WHERE case_id = $1 AND rung = 2`, out.CaseID); err != nil {
		t.Fatal(err)
	}
	own := g.stepsOfRung(t, out.CaseID, 2)
	g.r.PgSetNow(func() time.Time { return own[0].DueAt.Add(time.Minute) })
	for range 2 {
		expired, err := g.s.ExpireSteps(ctx, g.r.Clock())
		if err != nil {
			t.Fatal(err)
		}
		if err := g.r.ClimbExpired(ctx, expired); err != nil {
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
		!strings.Contains(info, "The check-in found:\n\n> ") || rec != "the card's work is done on main" || asks[0].Detail["recommended_by"] != "the check-in" {
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
		g.r.PgSetStore(projectStore{g.s})
		cardForge, _ := newAlertForgejo(t)
		g.r.ChatBudget = &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: cardForge, Timeline: g.r.PgTimeline()}
		ctx := context.Background()
		if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
			t.Fatal(err)
		}
		chat := g.spendChat(t)
		if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur - $2 WHERE id = $1`, chat.ID, left); err != nil {
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
		if err := g.s.PgDB().QueryRowContext(ctx, `SELECT count(*) FROM cases WHERE kind = 'judgment_budget'`).Scan(&cases); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("not judged, judgment budget spent: the chat task %s has %.2f of 10.00 EUR left", chat.ID, left)
		if out.Rung != 3 || len(g.rows(t, EventCaseClimbed)) != 0 || strings.Contains(c.Problem, "not judged") || cases != 1 ||
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
	if !strings.Contains(string(blob), `"preselected":{"action":"close"`) {
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
		Detail: map[string]any{"case_id": id, "rung": 2, "kind": "other", "problem": textutil.Truncate(g.r.PgScrub(c.Problem), 300)}}); err != nil {
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
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET plan_name = $1 WHERE id = 't3'`, "plan "+secretURL+" "+secretBearer); err != nil {
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
	g.r.PgSetStore(brokenReads{g.s})
	g.r.PgSetNow(func() time.Time { return time.Now().Add(time.Minute) })
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

// Register entry 7: at an agent-time cap rung 2 is offered the extend that
// raises agent time, a replan is offered and not refused, and a probe that
// buys sessions alone is not what the extend lists.
func TestRung2OffersTheExtendAtAnAgentTimeCap(t *testing.T) {
	f := CaseFacts{Kind: CaseAgentTimeCap, PROpen: true, Head: "h2", Checks: "green", ReviewApproved: true, FloorFree: true,
		SessionCap: 3, Attempts: 1, AgentTimeS: 1200, AgentTimeCapS: 1200, SessionLeastS: 300,
		PoolLeft: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour, EUR: 20}}
	var names []string
	for _, a := range Offered(f, 2) {
		names = append(names, a.Name)
	}
	if !slices.Contains(names, ActExtend) || !slices.Contains(names, ActReplan) {
		t.Fatalf("rung 2 is offered %v, want extend and replan among them", names)
	}
	if why := (&Runner{}).PgRungStatement(Case{Kind: CaseAgentTimeCap, Problem: "agent time"}, f, "digest", nil, 2); !strings.Contains(why, "\n- extend\n- ") {
		t.Fatalf("the statement lists %q, want extend as offered", why)
	}
}

// Register entry 50, first half: a planner that stopped for time is extended
// by rung 2 with the cut as its guidance: the extend is offered, the
// draw of agent time is taken, and the session's note carries the cut.
func TestRung2ExtendsAPlannerWithTheCutAsGuidance(t *testing.T) {
	const cut = "land one card compiling, then one card per proof"
	f := CaseFacts{Kind: CaseAgentTimeCap, Planner: true, FloorFree: true, SessionCap: 3, Attempts: 1,
		AgentTimeS: 1200, AgentTimeCapS: 1200, SessionLeastS: 300, PoolLeft: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour}}
	if offered := Offered(f, 2); !slices.ContainsFunc(offered, func(a Action) bool { return a.Name == ActExtend }) {
		t.Fatalf("rung 2 is offered %v, want extend among them", offered)
	}
	extend, _ := ActionByName(ActExtend)
	res := Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 1800}, Note: cut}
	if why := extend.PgRefusal(f, 2, res); why != "" {
		t.Fatalf("rung 2's extend of a planner with the cut is refused: %s", why)
	}
	if note := (&Runner{}).PgSessionNote(f, ActInput{Res: res}); !strings.Contains(note, cut) {
		t.Fatalf("the session's note %q lacks the cut", note)
	}
}

// From Task 2.15: rung 2's resume check weighs a cap that binds before the
// card's budget. A day cap reached and the budget short: resume is not
// offered as if a session could start now, and its refusal names the cap.
func TestRung2DoesNotOfferResumeWhileACapBinds(t *testing.T) {
	until := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	line, _, _ := strings.Cut(CapWaitReason(policy.RefusedDayCap, "the gateway", "the day's cap", until), "\n")
	f := CaseFacts{Kind: CaseSessionCap, FloorEUR: 1, LeftEUR: 0.2, SessionCap: 3, Attempts: 1, AgentTimeS: 100, AgentTimeCapS: 1200, SessionLeastS: 300,
		CapWait: line, PoolLeft: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour}}
	resume, _ := ActionByName(ActResume)
	why := resume.PgRefusal(f, 2, Resolution{Action: ActResume})
	if why == "" || !strings.Contains(why, "a cap binds before a session can start, so wait for it to turn or lift it: "+line) {
		t.Fatalf("resume refusal = %q, want the day cap's line", why)
	}
	for _, a := range Offered(f, 2) {
		if a.Name == ActResume || a.Name == ActContinue || a.Name == ActHandover {
			t.Fatalf("rung 2 is offered %s while the day cap binds", a.Name)
		}
	}
	f.CapWait = ""
	if why := resume.PgRefusal(f, 2, Resolution{Action: ActResume}); strings.Contains(why, "a cap binds") {
		t.Fatalf("resume refusal = %q without a cap wait", why)
	}
}

func (g monitorRig) rung1Off() {
	rung1Off(g.m.Runner)
	g.m.Policy = func() policy.Tree { return g.m.Runner.PgCfg().Policy().Tree }
}

// A wake's case_climbed row whose step ended since wakes nobody, and an
// ended step's face is no wake either; a live step's row wakes.
func TestStaleCaseClimbedWakesNobody(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	g.afterRung2(t, out.CaseID)
	g.r.PgSetNow(func() time.Time { return time.Now().Add(time.Hour) })
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
	want := fmt.Sprintf("`case_id` %d is no ask of this card. Its open asks, by `case_id`: %d.", other.ID, out.CaseID)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
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
		if err := g.r.PgTimeline().Append(ctx, "t1", "checkin", map[string]any{"decision": "message", "moment": "stalled", "guidance": guidance}); err != nil {
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
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET track = 'plan', issue_number = 1547 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateTask(ctx, Task{ID: "c1587", Kind: "agent", Status: "running", Objective: "the size bound", Repository: "demo-app/demo-app", IssueNumber: 1587,
		Track: policy.TrackDirect, Source: SourcePlan, Parent: "t1", PlanKey: "api", Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	const cut = "land one card that compiles, then one card per proof"
	work := replanText(&Trip{Kind: TripSize, Detail: "1774 lines against 1742"},
		Decision{Action: ActionReplan, Reason: strings.Repeat("r", 354), Guidance: cut + " " + secretURL + " " + strings.Repeat("é", 450)})
	if _, err := g.r.PgTimeline().AppendUnlessPending(ctx, "t1", stepReplan, CutReplanDetail("c1587", work, 1), replanClosers...); err != nil {
		t.Fatal(err)
	}
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, TimeCapReached+"\nlast: no session produced a result")
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
	other, _, err := openAtHuman(ctx, g.s, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseHumanCard, StateKey: "h", Problem: "a human card"})
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
// case keeps the mark: the agent's view says the factory found it not
// judged, never that a rung judged it, and the problem stays its own.
func TestRung1ThatDidNotJudgeLeavesTheMark(t *testing.T) {
	for _, found := range []string{"rung 1 could not run: the gateway refused the call for money", "the rung did not answer within 35m"} {
		g := newRungRig(t, "success")
		g.r.PgSetPrices(nil)
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
		task, _, _ := g.r.PgTask(ctx, "t1")
		if _, err := g.r.Climb(ctx, task, c, CaseFacts{}, 1, found); err != nil {
			t.Fatal(err)
		}
		if got := caseByID(t, g.s, c.ID); got.Problem != "stuck" {
			t.Fatalf("%q: problem %q, want it unmarked", found, got.Problem)
		}
		if info := g.infoOf(t, c.ID); !strings.Contains(info, "The project's judgment budget has") || strings.Contains(info, "not judged") || strings.Contains(info, "t-chat") {
			t.Fatalf("%q: info %q, want the mark", found, info)
		}
	}
}

// The scrub of a supervisor's message sits in the digest's own render, which
// the card's life page and the monitor's facts show unscrubbed by anything
// else.
func TestDigestRenderMasksASupervisorsMessage(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if err := g.r.PgTimeline().Append(ctx, "t1", "checkin", map[string]any{"decision": "message", "moment": "stalled", "guidance": "go on " + secretURL + " " + secretBearer}); err != nil {
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

// The spent check on the rung 2 path prices the call on big, the class the
// wake runs on, whatever premium costs: a call needs 0.01536 EUR for its
// 1024 tokens at big's 1.5e-5 EUR per output token, so 0.05 EUR left starts
// rung 2 with premium priced at 1e-4 (0.1024 EUR) or not.
func TestRung2SpentCheckPricesBig(t *testing.T) {
	for _, premium := range []bool{false, true} {
		g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
		g.r.PgSetStore(projectStore{g.s})
		ctx := context.Background()
		if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET required_capabilities = '[]'::jsonb WHERE id = 't1'`); err != nil {
			t.Fatal(err)
		}
		if premium {
			v := g.r.PgCfg().Policy()
			v.Document.Prices.MaxOutputTokens = map[string]int{policy.Premium: 24576}
			g.r.PgSetPolicy(func() policy.Version { return v })
			g.r.PgSetPrices(aliasPrices{"big": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}, "premium": {In: 1e-5, Out: 1e-4, MaxOutput: 24576}})
		}
		chat := g.spendChat(t)
		if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur - 0.05 WHERE id = $1`, chat.ID); err != nil {
			t.Fatal(err)
		}
		out := g.trip(t, tripCap)
		if out.Rung != 2 {
			t.Fatalf("premium priced %v: outcome %+v; want rung 2 to start on big's floor", premium, out)
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
	if info := g.infoOf(t, out.CaseID); !strings.Contains(info, "The factory agent did not answer within 35m.") || !strings.Contains(info, "The check-in found:\n\n> the check-in sends this to a human: "+CheckSpend) {
		t.Fatalf("info = %q, want the moment's reason kept", info)
	}

	h, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	h.chat.onCall = func() {}
	h.chat.answers = []CheckinResponse{h.handUp(t)}
	two := h.trip(t, tripCap)
	h.settle()
	ctx := context.Background()
	if _, err := h.s.PgDB().ExecContext(ctx, `UPDATE case_steps SET found = 'the rung did not answer within 35m' WHERE case_id = $1 AND rung = 1`, two.CaseID); err != nil {
		t.Fatal(err)
	}
	h.afterRung2(t, two.CaseID)
	if info := h.infoOf(t, two.CaseID); !strings.Contains(info, "The check-in did not answer within 35m.") || strings.Contains(info, "found: the rung did not answer") {
		t.Fatalf("info = %q, want rung 1's expiry in the same words", info)
	}
}

// pokeRows are the inbox rows the Poker wrote about card t1, by the kind of
// the event each was born of, after one pass over every event on record.
func (g rungRig) pokeRows(t *testing.T) []string { return g.pokeRowsOf(t, "t1") }

// pokeRowsOf is pokeRows of card taskID.
func (g rungRig) pokeRowsOf(t *testing.T, taskID string) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `TRUNCATE chat_poke_cursor; INSERT INTO chat_poke_cursor (event_id) VALUES (0)`); err != nil {
		t.Fatal(err)
	}
	poker := &Poker{Store: g.s, Current: g.r.PgCfg().Policy, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := poker.Pass(ctx); err != nil {
		t.Fatalf("Poker.Pass: %v", err)
	}
	rows, err := g.s.PgDB().QueryContext(ctx, `SELECT birth_kind FROM chat_messages WHERE task_id = $2 AND kind = $1 ORDER BY id`, MessageEscalation, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// A plan's criteria case climbs as a trip's does. Its request's row opens
// it at rung 0 with no face and no poke; the case-wake pass runs rung 0 on
// the done plan, where main has not moved past the gate's two cut runs, so
// no rerun, and hands it to rung 2, whose one wake turn takes it. Rung 2's
// hand-up faces it: the case at the human with its steps of rung 0, 2 and 3,
// one comment holding the request's words as factoryd wrote them, an ask
// naming the case and the request, the register's face of the gap with
// rung 2's finding as the agent's view and its grant suggested,
// one poke, the ask's, and no case_faced, for no run waits on the plan.
func TestACriteriaCaseClimbsToRung2AndFacesItsHandUp(t *testing.T) {
	g, w, starter, stub, c := planCriteriaRig(t, "m1", "m1", "m1")
	ctx := context.Background()
	if len(g.forge.comments) != 0 || len(g.pokeRows(t)) != 0 {
		t.Fatalf("comments %q, pokes %v at the opening; want none", g.forge.comments, g.pokeRows(t))
	}
	g.wakePass(t)
	if rows := g.rows(t, EventCriteriaGateRun); len(rows) != 2 {
		t.Fatalf("gate rows = %+v, want the two cut runs alone", rows)
	}
	// the rig's process has no plan track, so the plan card's config reads
	// partly: rung 0's other rows take nothing either way
	steps := g.steps(t, c.ID)
	if len(steps) != 2 || steps[0].Rung != 0 || steps[0].Found != "facts read partly: config, so rung 0 took nothing" || steps[1].Rung != 2 || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0's hand-up and rung 2 in progress", steps)
	}
	if climbed := g.rows(t, EventCaseClimbed); len(climbed) != 1 || climbed[0].Detail["case_id"] != float64(c.ID) {
		t.Fatalf("case_climbed rows = %+v, want one for case %d", climbed, c.ID)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 1 || calls[0].birth != EventCaseClimbed || !slices.Equal(calls[0].cases, []int64{c.ID}) {
		t.Fatalf("wakes = %+v, want one case_climbed turn on case %d", calls, c.ID)
	}

	if _, err := g.r.HandUpRung2(ctx, "t1", c.ID, "k1 to k3 still fail on main",
		RungTwoRecommendation{Action: ActGrantCriteria, Grounds: "one more card can make the pages load"}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	got := caseByID(t, g.s, c.ID)
	steps = g.steps(t, c.ID)
	if got.Status != CaseAtHuman || len(steps) != 3 || steps[0].Rung != 0 || steps[1].Rung != 2 || steps[2].Rung != 3 {
		t.Fatalf("case %s, steps %+v; want it at the human with steps of rung 0, 2 and 3", got.Status, steps)
	}
	if len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], "Every card of the plan has ended with a criterion still unmet.\\n\\n- k1 (test): the page k1 loads") ||
		strings.Contains(g.forge.comments[0], "```") {
		t.Fatalf("comments = %q, want one with the request's words, unfenced", g.forge.comments)
	}
	asks := g.asks(t)
	if len(asks) != 1 || asks[0].Detail["case_id"] != float64(c.ID) || asks[0].Detail["approval"] != float64(stub.approval) ||
		asks[0].Detail["case_kind"] != string(CasePlanCriteria) ||
		asks[0].Detail["ask"] != "Plan “x” finished, but 3 of 3 promises are not met. Settle each one." ||
		asks[0].Detail["why"] != "Until all 3 are settled, the plan stays open and holds production deploys." ||
		asks[0].Detail["info"] != "The factory agent found:\n\n> k1 to k3 still fail on main" ||
		asks[0].Detail["recommendation"] != "one more card can make the pages load" || asks[0].Detail["recommended_by"] != "the factory agent" {
		t.Fatalf("asks = %+v, want one with the gap's ask and why, rung 2's finding and its recommendation", asks)
	}
	var offered []string
	offers, _ := asks[0].Detail["offers"].([]any)
	for _, o := range offers {
		offered = append(offered, fmt.Sprint(o.(map[string]any)["value"]))
	}
	if pre, _ := asks[0].Detail["preselected"].(map[string]any); pre["action"] != ActGrantCriteria || !slices.Equal(offered, []string{ActGrantCriteria, ActAnswerCriteria + "/waive"}) {
		t.Fatalf("the face suggests %v among %v, want the grant first, then the waiver", asks[0].Detail["preselected"], offered)
	}
	if pokes := g.pokeRows(t); !slices.Equal(pokes, []string{EventQuestionAsked}) {
		t.Fatalf("pokes = %v, want the face's ask alone", pokes)
	}
	if faced := g.signalsOf(SignalCaseFaced); len(faced) != 0 {
		t.Fatalf("case_faced signals = %+v, want none: no run waits on the done plan", faced)
	}
}

// Rung 2 grants one more bounded card on the criteria case: the grant
// decides the request and resolves the case on rung 2, not withdrawn, and
// files no card yet: the delta pass drafts it from the grant's dues.
func TestRung2GrantsACriteriaCase(t *testing.T) {
	g, _, _, stub, c := planCriteriaRig(t, "m1", "m1", "m1")
	g.atAutonomy(policy.AutonomyAutonomous)
	g.wakePass(t)
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActGrantCriteria}}); err != nil {
		t.Fatalf("rung 2's grant: %v", err)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Action != ActGrantCriteria || got.Resolution.Rung != 2 {
		t.Fatalf("case = %+v, want it resolved by rung 2's grant", got)
	}
	cards, err := g.s.PlanCards(context.Background(), "t1")
	if err != nil || len(cards) != 1 || len(g.forge.comments) != 0 {
		t.Fatalf("plan cards %+v (%v), comments %q; want t1-int alone and no face", cards, err, g.forge.comments)
	}
}

// A plan's criteria case outlives its done plan card: the case-wake pass
// climbs it, its rung 2 step past its bound ends in the pass and climbs it
// on, and its face stands with no withdrawal, at the human with one
// comment. The heal pass leaves it and opens no second case.
func TestACriteriaCaseOnADonePlanReachesItsFace(t *testing.T) {
	g, _, _, _, c := planCriteriaRig(t, "m1", "m1", "m1")
	ctx := context.Background()
	g.wakePass(t)
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE case_steps SET due_at = now() - interval '1 minute', started_at = now() - interval '36 minutes'
		WHERE case_id = $1 AND rung = 2`, c.ID); err != nil {
		t.Fatal(err)
	}
	g.wakePass(t)
	if got := caseByID(t, g.s, c.ID); got.Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("case %s with %d comments, want it at the human with one face", got.Status, len(g.forge.comments))
	}
	rep, err := g.s.ReconcileCases(ctx, "demo-app/demo-app")
	if err != nil || rep.Opened != 0 || rep.Withdrawn != 0 {
		t.Fatalf("ReconcileCases = %+v, %v; want nothing opened or withdrawn", rep, err)
	}
	if open, _ := g.s.OpenCasesOf(ctx, "t1"); len(open) != 1 || open[0].ID != c.ID || open[0].Status != CaseAtHuman {
		t.Fatalf("open cases = %+v, want case %d at the human alone", open, c.ID)
	}
}

// Main moved past the gate's last run: rung 0 runs the gate once more and
// holds the case for it, with its one step and no rung 2. The gate meets
// every criterion, and the close check's resolve ends the case resolved by
// factoryd at rung 0, its one step the rerun's, with no step of rung 2 or 3.
func TestACriteriaCaseTheRerunMeetsEndsAtRungZero(t *testing.T) {
	g, _, _, _, c := planCriteriaRig(t, "m2", "m1", "m1")
	ctx := context.Background()
	g.wakePass(t)
	steps := g.steps(t, c.ID)
	if len(steps) != 1 || steps[0].Rung != 0 || steps[0].Found != "criteria gate run again at main m2" {
		t.Fatalf("steps = %+v, want rung 0's rerun step alone", steps)
	}
	held := caseByID(t, g.s, c.ID)
	if held.Status != CaseOpen || held.OwnerRung != 0 || held.NextWakeAt == nil || time.Until(*held.NextWakeAt) < 50*time.Minute {
		t.Fatalf("case = %+v, want it open, held at rung 0 for the gate's run", held)
	}
	if err := g.s.ResolveApprovals(ctx, "t1", ApprovalCriteria, "approved", FactoryDecider, "every criterion is met or waived"); err != nil {
		t.Fatal(err)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.By != FactoryDecider || got.Resolution.Rung != 0 {
		t.Fatalf("case = %+v, want it resolved by factoryd at rung 0", got)
	}
	if steps := g.steps(t, c.ID); len(steps) != 1 || steps[0].Rung != 0 {
		t.Fatalf("steps = %+v, want the rerun's alone", steps)
	}
}

// pokeChoices are the choices of card t1's inbox rows, as stored, oldest
// first: the rows pokeRows wrote.
func (g rungRig) pokeChoices(t *testing.T) []string {
	t.Helper()
	rows, err := g.s.PgDB().QueryContext(context.Background(), `SELECT COALESCE(choices::text, '') FROM chat_messages WHERE task_id = 't1' AND kind = $1 ORDER BY id`, MessageEscalation)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// A merge at full auto climbs as a trip's case does. Its request's row
// opens it at rung 0 with no face and no poke, and the Waker wakes no turn
// on that row; the case-wake pass runs rung 0, where no row fits a merge,
// and hands it to rung 2, whose one wake turn takes it. Rung 2's
// approve_merge runs the console's approve: the run hears approved from
// factory-chat for pull request 9, the case is resolved by factory-chat at
// rung 2, and no human is asked: no comment, no ask, no poke.
func TestAMergeAtFullAutoClimbsToRung2WhichApprovesIt(t *testing.T) {
	g, w, starter, _, c := mergeWakeRig(t, policy.AutonomyFullAuto)
	ctx := context.Background()
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 0 || len(g.forge.comments) != 0 || len(g.pokeRows(t)) != 0 {
		t.Fatalf("wakes %+v, comments %q, pokes %v at the opening; want none", calls, g.forge.comments, g.pokeRows(t))
	}
	g.wakePass(t)
	steps := g.steps(t, c.ID)
	if len(steps) != 2 || steps[0].Rung != 0 || steps[0].Found != "no resolver fits" || steps[1].Rung != 2 || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0's hand-up and rung 2 in progress", steps)
	}
	if climbed := g.rows(t, EventCaseClimbed); len(climbed) != 1 || climbed[0].Detail["case_id"] != float64(c.ID) {
		t.Fatalf("case_climbed rows = %+v, want one for case %d", climbed, c.ID)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 1 || calls[0].birth != EventCaseClimbed || !slices.Equal(calls[0].cases, []int64{c.ID}) {
		t.Fatalf("wakes = %+v, want one case_climbed turn on case %d", calls, c.ID)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActApproveMerge},
		Work: g.approveWork(g.signals, ChatUser)}); err != nil {
		t.Fatalf("rung 2's approve: %v", err)
	}
	if approved := g.signalsOf(SignalApproved); len(approved) != 1 || approved[0].taskID != "t1" || approved[0].sig.Author != ChatUser || approved[0].sig.PR != 9 {
		t.Fatalf("approved signals = %+v, want t1 approved by %s for #9", approved, ChatUser)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.By != ChatUser || got.Resolution.Rung != 2 || got.Resolution.Action != ActApproveMerge {
		t.Fatalf("case = %+v, resolution %+v; want it resolved by %s at rung 2 with approve_merge", got, got.Resolution, ChatUser)
	}
	if len(g.forge.comments) != 0 || len(g.asks(t)) != 0 || len(g.pokeRows(t)) != 0 {
		t.Fatalf("comments %q, asks %+v, pokes %v; want none", g.forge.comments, g.asks(t), g.pokeRows(t))
	}
}

// On merge rule human rung 2 decides no merge: at autonomous its
// approve_merge and deny_merge are refused with the hand-up's words, and
// nothing runs. Its hand-up with approve_merge as the recommendation faces
// the case beside the card's run: at the human, the card still in review,
// no comment and no case_faced; one ask asks the merge with the merge
// gate's class as its why, rung 2's finding in its info and its
// recommendation, and one poke, the ask's, offers Approve.
func TestAMergeOnMergeRuleHumanIsHandedUpWithRung2sRecommendation(t *testing.T) {
	g, _, _, stub, c := mergeWakeRig(t, policy.AutonomyAutonomous)
	ctx := context.Background()
	g.wakePass(t)
	const refusal = "the merge rule is human at `autonomous`: hand the case up (`hand_up`) with `approve_merge` or `deny_merge` as the recommendation"
	for _, res := range []Resolution{{Action: ActApproveMerge}, {Action: ActDenyMerge, Args: map[string]any{"action": "reject"}, Note: "the tests miss the rename"}} {
		_, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: res})
		var refused ErrActRefused
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("rung 2's %s = %v, want the refusal %q", res.Action, err, refusal)
		}
	}
	if len(stub.calls) != 0 || caseByID(t, g.s, c.ID).Status != CaseOpen {
		t.Fatalf("board calls %d, case %s; want none and the case open", len(stub.calls), caseByID(t, g.s, c.ID).Status)
	}
	if _, err := g.r.HandUpRung2(ctx, "t1", c.ID, "the review approved h2 and the checks are green",
		RungTwoRecommendation{Action: ActApproveMerge, Grounds: "the change is small and covered"}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	task, _, err := g.s.Task(ctx, "t1")
	if got := caseByID(t, g.s, c.ID); err != nil || got.Status != CaseAtHuman || task.Status != "in-review" {
		t.Fatalf("case %s, card %s (%v); want the case at the human and the card in review", got.Status, task.Status, err)
	}
	if len(g.forge.comments) != 0 || len(g.signalsOf(SignalCaseFaced)) != 0 {
		t.Fatalf("comments %q, case_faced %+v; want none", g.forge.comments, g.signalsOf(SignalCaseFaced))
	}
	asks := g.asks(t)
	if len(asks) != 1 {
		t.Fatalf("asks = %+v, want one", asks)
	}
	face := asks[0].Detail
	offers, _ := face["offers"].([]any)
	suggested, _ := face["preselected"].(map[string]any)
	if len(offers) == 0 || face["case_id"] != float64(c.ID) || face["ask"] != "Merge “x”?" ||
		face["why"] != "The factory's rules let only a human merge this change." || offers[0].(map[string]any)["value"] != ActApproveMerge ||
		!strings.HasPrefix(face["info"].(string), "The factory agent found:\n\n> the review approved h2 and the checks are green") ||
		suggested["action"] != ActApproveMerge || face["recommendation"] != "the change is small and covered" || face["recommended_by"] != "the factory agent" {
		t.Fatalf("asks = %+v, want one asking the merge, its merge the first answer, with rung 2's finding and recommendation", asks)
	}
	if pokes, choices := g.pokeRows(t), g.pokeChoices(t); !slices.Equal(pokes, []string{EventQuestionAsked}) || !slices.Equal(choices, []string{`["Merge it"]`}) {
		t.Fatalf("pokes %v with choices %v, want the face's ask alone, offering the merge", pokes, choices)
	}
}

// lostRun is the workflows of a card whose run is gone: every signal fails.
type lostRun struct{ Workflows }

func (lostRun) Signal(context.Context, string, string, TaskSignal) error {
	return errors.New("no workflow for t1")
}

// Rung 2's approve at full auto whose signal fails is undone: the request
// pends again, and the case is open at rung 0 and due, rung 2's step ended
// handed up with the undo's words and a step of rung 0 by factoryd saying
// the same. One case-wake pass faces it with no second rung 2 turn: rung 2
// worked the case, so its finding, the undo, is the face's info.
func TestAnUndoneRung2ApprovalReachesItsFaceInOneWakePass(t *testing.T) {
	g, _, _, stub, c := mergeWakeRig(t, policy.AutonomyFullAuto)
	ctx := context.Background()
	g.wakePass(t)
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActApproveMerge},
		Work: g.approveWork(lostRun{}, ChatUser)}); err == nil || !strings.Contains(err.Error(), "no workflow for t1") {
		t.Fatalf("rung 2's approve = %v, want the signal's failure", err)
	}
	pending, err := g.s.ApprovalRequests(ctx, "pending")
	if err != nil || !slices.ContainsFunc(pending, func(q ApprovalRequest) bool { return q.ID == stub.approval }) {
		t.Fatalf("pending requests = %+v (%v), want request %d pending again", pending, err, stub.approval)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseOpen || got.OwnerRung != 0 || got.NextWakeAt == nil || time.Until(*got.NextWakeAt) > 0 || got.Resolution != nil {
		t.Fatalf("case = %+v, want it open at rung 0, unresolved and due", got)
	}
	const undone = "approval undone: no workflow for t1"
	steps := g.steps(t, c.ID)
	if len(steps) != 3 || steps[1].Rung != 2 || steps[1].Outcome != StepHandedUp || steps[1].Found != undone ||
		steps[2].Rung != 0 || steps[2].Actor != FactoryDecider || steps[2].Outcome != StepHandedUp || steps[2].Found != undone {
		t.Fatalf("steps = %+v, want rung 0's, rung 2's handed up with %q, and rung 0's undo", steps, undone)
	}
	g.wakePass(t)
	if got := caseByID(t, g.s, c.ID); got.Status != CaseAtHuman {
		t.Fatalf("case %s after one wake pass, want it at the human", got.Status)
	}
	if n := len(slices.DeleteFunc(g.steps(t, c.ID), func(s CaseStep) bool { return s.Rung != 2 })); n != 1 || len(g.rows(t, EventCaseClimbed)) != 1 {
		t.Fatalf("%d rung 2 steps and %d case_climbed rows, want one of each", n, len(g.rows(t, EventCaseClimbed)))
	}
	if asks := g.asks(t); len(asks) != 1 || !strings.Contains(asks[0].Detail["info"].(string), "The factory agent found:\n\n> "+undone) {
		t.Fatalf("asks = %+v, want one whose info carries the undo", asks)
	}
}

// A plan's question at full auto climbs as a merge does. The planner's row
// opens its case at rung 0, due at the row, with no step, no comment, no
// poke and no wake; the case-wake pass runs rung 0, where no row fits a
// plan's question, and hands the case to rung 2, whose one wake turn takes
// it, told to end it with the plan's answer. Rung 2's answer through
// AnswerPlan, by the factory agent, signals plan_answered, and the
// console's row, whose actor in a wake is the venue's human, resolves the
// case by the factory agent at rung 2, as the stored answer names it. No
// human is asked: no comment, no face, no poke.
func TestAPlanQuestionAtFullAutoClimbsToRung2WhichAnswersIt(t *testing.T) {
	g, w, starter, _ := planWakeRig(t, policy.AutonomyFullAuto, 1, Plan{Summary: "the store"},
		PlanQuestion{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}, Recommended: "Postgres", Grounds: "it is shared"})
	ctx := context.Background()
	c := g.liveCase(t, CasePlanQuestion)
	asked := g.asks(t)
	if len(asked) != 1 || c.Status != CaseOpen || c.OwnerRung != 0 || c.NextWakeAt == nil || !c.NextWakeAt.Equal(asked[0].At) || len(g.steps(t, c.ID)) != 0 {
		t.Fatalf("case = %+v with steps %+v, asks %+v; want it open at rung 0, due at the planner's row, with no step", c, g.steps(t, c.ID), asked)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 0 || len(g.forge.comments) != 0 || len(g.pokeRows(t)) != 0 {
		t.Fatalf("wakes %+v, comments %q, pokes %v at the opening; want none", calls, g.forge.comments, g.pokeRows(t))
	}
	g.wakePass(t)
	// rung 0 takes no plan answer: it hands the case up
	steps := g.steps(t, c.ID)
	if len(steps) != 2 || steps[0].Rung != 0 || steps[0].Outcome != StepHandedUp || steps[1].Rung != 2 || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0's hand-up and rung 2 in progress", steps)
	}
	if climbed := g.rows(t, EventCaseClimbed); len(climbed) != 1 || climbed[0].Detail["case_id"] != float64(c.ID) {
		t.Fatalf("case_climbed rows = %+v, want one for case %d", climbed, c.ID)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	end := fmt.Sprintf("End case %d with answer on the plan's revision and question, with no case_id, or with attach_analysis, case_id %d and the recommended answer.", c.ID, c.ID)
	if calls := starter.recorded(); len(calls) != 1 || calls[0].birth != EventCaseClimbed || !slices.Equal(calls[0].cases, []int64{c.ID}) ||
		!strings.Contains(calls[0].text, end) {
		t.Fatalf("wakes = %+v, want one case_climbed turn on case %d that ends with %q", calls, c.ID, end)
	}
	if err := factoryd.AnswerPlan(ctx, g.s, g.signals, "t1", 1, 1, "", "Postgres", ChatUser); err != nil {
		t.Fatalf("rung 2's answer: %v", err)
	}
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: EventPlanQuestionAnswered,
		Actor: "sebastian", Via: ViaChat, Detail: map[string]any{"revision": 1, "index": 1, "answer": "Postgres", "decider": "agent"}}); err != nil {
		t.Fatal(err)
	}
	if answered := g.signalsOf(factoryd.SignalPlanAnswered); len(answered) != 1 || answered[0].taskID != "t1" || answered[0].sig.Author != ChatUser ||
		answered[0].sig.Revision != 1 || answered[0].sig.Index != 1 {
		t.Fatalf("plan_answered signals = %+v, want t1's question 1 of revision 1 by %s", answered, ChatUser)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.By != ChatUser || got.Resolution.Rung != 2 || got.Resolution.Action != CaseAnswer {
		t.Fatalf("case = %+v, resolution %+v; want it resolved by %s at rung 2 with answer", got, got.Resolution, ChatUser)
	}
	if steps := g.steps(t, c.ID); len(steps) != 2 || steps[1].Rung != 2 || steps[1].Outcome != StepResolved {
		t.Fatalf("steps = %+v, want rung 2's step ended with the case", steps)
	}
	if len(g.forge.comments) != 0 || len(g.asks(t)) != 1 || len(g.pokeRows(t)) != 0 {
		t.Fatalf("comments %q, asks %+v, pokes %v; want no face and no poke", g.forge.comments, g.asks(t), g.pokeRows(t))
	}
}

// A confirmation of a revision that takes merged work off main is the
// human's at every level. Rung 2 hands it up, and an answer no confirmation
// takes is refused naming the two it takes. With revise as its
// recommendation the case is at the human beside the plan's run: the card
// still waits for its answers, no case_faced goes out, and rung 2's step
// holds its decision with no action proposed. The face's row carries the
// revision, the index, the kind and the case's kind, rung 2's finding and
// recommendation; one comment on the plan's issue names the confirmation
// with that recommendation and that a reply asks the planner for a new
// revision; one poke, the face's, asks it.
func TestAPlanConfirmationThatTakesWorkOffMainIsHandedUp(t *testing.T) {
	const confirmation = "Confirmation of the spec and the criteria of plan revision 1: Confirm fixes the spec and the criteria; Request changes sends your words to the next planner round."
	g, _, _, _ := planWakeRig(t, policy.AutonomyFullAuto, 1, Plan{Summary: "the store", TakeOffMain: []string{"api"}},
		PlanQuestion{Text: confirmation + "\nCriteria: c1 (test).", Kind: QuestionConfirmation})
	ctx := context.Background()
	c := g.liveCase(t, CasePlanConfirmation)
	g.wakePass(t)
	_, err := g.r.HandUpRung2(ctx, "t1", c.ID, "x", RungTwoRecommendation{Action: ActApproveMerge, Grounds: "y"}, nil)
	var refused ErrActRefused
	if want := `"approve_merge" is no decision of a plan's confirmation: recommend confirm or revise`; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("the hand-up with approve_merge = %v, want %q", err, want)
	}
	if _, err := g.r.HandUpRung2(ctx, "t1", c.ID, "revision 1 takes the merged api card off main",
		RungTwoRecommendation{Action: factoryd.DecisionRevise, Grounds: "keep the api card on main"}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	task, _, err := g.s.Task(ctx, "t1")
	if got := caseByID(t, g.s, c.ID); err != nil || got.Status != CaseAtHuman || task.Status != "waiting-human" {
		t.Fatalf("case %s, card %s (%v); want the case at the human and the card waiting", got.Status, task.Status, err)
	}
	if faced := g.signalsOf(SignalCaseFaced); len(faced) != 0 {
		t.Fatalf("case_faced %+v; want none", faced)
	}
	if steps := g.stepsOfRung(t, c.ID, 2); len(steps) != 1 || steps[0].Outcome != StepHandedUp || steps[0].Decision != factoryd.DecisionRevise ||
		steps[0].Grounds != "keep the api card on main" || len(steps[0].Did) != 0 {
		t.Fatalf("rung 2's steps = %+v, want one handed up with the decision revise and no action", steps)
	}
	asks := g.asks(t)
	if len(asks) != 2 {
		t.Fatalf("asks = %+v, want the planner's row and the face's", asks)
	}
	face := asks[1].Detail
	suggested, _ := face["preselected"].(map[string]any)
	if face["case_id"] != float64(c.ID) || face["case_kind"] != string(CasePlanConfirmation) || face["revision"] != float64(1) || face["index"] != float64(1) ||
		face["kind"] != QuestionConfirmation || face["ask"] != "Confirm plan “x” so its cards can start?" ||
		!strings.HasPrefix(face["info"].(string), "The factory agent found:\n\n> revision 1 takes the merged api card off main") ||
		suggested["action"] != factoryd.AnswerRevisePlan || face["recommendation"] != "keep the api card on main" || face["recommended_by"] != "the factory agent" ||
		face["comment"] != float64(501) {
		t.Fatalf("the face's row = %+v", face)
	}
	want := "Revision 1 of the plan waits for the humans' answers in Needs you on the factory console.\n\n" +
		"1. " + confirmation + "\n   The factory agent suggests “Ask for changes”: keep the api card on main\n\n" +
		"A reply on this issue asks the planner for a new revision instead. Closing the issue ends the plan.\n<!-- factoryd questions of plan t1 revision 1 -->"
	if bodies := g.commentBodies(t); len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("comments %q, want one:\n%s", bodies, want)
	}
	if pokes := g.pokeRows(t); !slices.Equal(pokes, []string{EventQuestionAsked}) {
		t.Fatalf("pokes %v, want the face's ask alone", pokes)
	}
}

// The questions of one revision share one face comment. Three questions of
// revision 2 handed up in turn: the first hand-up posts the comment, which
// names its question with rung 2's recommendation; the next two edit it,
// and after the third it names all three in the order of their numbers.
// Rung 2's recommendation must be one of a question's choices.
func TestTheQuestionsOfARevisionShareOneFaceComment(t *testing.T) {
	qs := []PlanQuestion{
		{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}, Recommended: "Postgres", Grounds: "it is shared"},
		{Text: "Which queue?", Choices: []string{"NATS", "none"}, Recommended: "none", Grounds: "one process"},
		{Text: "Which cache?", Choices: []string{"none", "Redis"}, Recommended: "none", Grounds: "no hot path"},
	}
	g, _, _, _ := planWakeRig(t, policy.AutonomyAutonomous, 2, Plan{Summary: "the store"}, qs...)
	ctx := context.Background()
	open, err := g.s.OpenCasesOf(ctx, "t1")
	if err != nil || len(open) != 3 {
		t.Fatalf("open cases = %+v, %v; want the three questions'", open, err)
	}
	g.wakePass(t)
	_, err = g.r.HandUpRung2(ctx, "t1", open[0].ID, "x", RungTwoRecommendation{Action: "MySQL", Grounds: "y"}, nil)
	var refused ErrActRefused
	if want := `"MySQL" is no choice of this plan question: recommend one of Postgres and SQLite`; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("the hand-up with MySQL = %v, want %q", err, want)
	}
	recs := []RungTwoRecommendation{{Action: "SQLite", Grounds: "one process writes"}, {Action: "NATS", Grounds: "two services"}, {Action: "Redis", Grounds: "the board is hot"}}
	head := "Revision 2 of the plan waits for the humans' answers in Needs you on the factory console.\n\n"
	lines := []string{"1. Which database?\n   The factory agent suggests “SQLite”: one process writes\n",
		"2. Which queue?\n   The factory agent suggests “NATS”: two services\n",
		"3. Which cache?\n   The factory agent suggests “Redis”: the board is hot\n"}
	const tail = "\nA reply on this issue asks the planner for a new revision instead. Closing the issue ends the plan.\n<!-- factoryd questions of plan t1 revision 2 -->"
	for i, c := range open {
		if _, err := g.r.HandUpRung2(ctx, "t1", c.ID, "read the plan", recs[i], nil); err != nil {
			t.Fatalf("hand-up %d: %v", i+1, err)
		}
		want := head + strings.Join(lines[:i+1], "") + tail
		if bodies := g.commentBodies(t); len(bodies) != 1 || bodies[0] != want {
			t.Fatalf("after hand-up %d: comments %q, want one:\n%s", i+1, bodies, want)
		}
	}
	for _, a := range g.asks(t)[3:] {
		if a.Detail["comment"] != float64(501) {
			t.Fatalf("face row %+v, want it on comment 501", a.Detail)
		}
	}
}

// The plan's request, denied by a decider's request for changes, ends its
// live cases with that decider's revise: the factory agent's at rung 2,
// with rung 2's step, a human's at rung 3. The factory's denial decides
// nothing: it withdraws them.
func TestADeniedPlanRequestEndsItsCases(t *testing.T) {
	for _, tc := range []struct {
		by     string
		status string
		rung   int
	}{{ChatUser, CaseResolved, 2}, {"sebastian", CaseResolved, 3}, {FactoryDecider, CaseWithdrawn, 0}} {
		t.Run(tc.by, func(t *testing.T) {
			g, _, _, id := planWakeRig(t, policy.AutonomyFullAuto, 1, Plan{Summary: "the store"},
				PlanQuestion{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}}, PlanQuestion{Text: "Which queue?", Choices: []string{"NATS", "none"}})
			ctx := context.Background()
			g.wakePass(t)
			if err := g.s.DecideApproval(ctx, id, factoryd.ApprovalPlan, "denied", tc.by, "changes requested: split the API card"); err != nil {
				t.Fatalf("DecideApproval: %v", err)
			}
			cases, err := g.s.CasesOf(ctx, "t1")
			if err != nil || len(cases) != 2 {
				t.Fatalf("cases = %+v, %v; want the two questions'", cases, err)
			}
			for _, c := range cases {
				want := &CaseResolution{By: tc.by, Rung: tc.rung, Action: CaseAnswer,
					Args: map[string]any{"answer": "changes requested: split the API card", "decision": factoryd.DecisionRevise}}
				if tc.by == FactoryDecider {
					want = &CaseResolution{By: FactoryDecider, Action: "withdraw", Reason: "changes requested: split the API card"}
				}
				if c.Status != tc.status || !reflect.DeepEqual(c.Resolution, want) {
					t.Fatalf("case = %+v, resolution %+v; want %s %+v", c, c.Resolution, tc.status, want)
				}
				if steps := g.stepsOfRung(t, c.ID, 2); len(steps) != 1 || steps[0].Outcome == StepInProgress {
					t.Fatalf("rung 2's steps of case %d = %+v, want its one step ended", c.ID, steps)
				}
			}
		})
	}
}

// The record names who decided a criteria case. When a human's waivers
// settled the plan's criteria, the close check's resolve ends the case
// with that human as the decider at rung 3, and the request names them too;
// when the gate's rows on main alone settled them, factoryd stays the
// decider at rung 0.
func TestTheCloseCheckNamesWhoSettledTheCriteria(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle func(t *testing.T, g rungRig)
		by     string
		rung   int
	}{
		{"a human's waivers", func(t *testing.T, g rungRig) {
			for _, id := range []string{"k1", "k2", "k3"} {
				if err := factoryd.DecideCriterion(context.Background(), g.s, "t1", id, "waived", "sebastian", "the page "+id+" is gone"); err != nil {
					t.Fatal(err)
				}
			}
		}, "sebastian", 3},
		{"the gate's rows on main", func(t *testing.T, g rungRig) {
			var rows []CriterionVerdict
			for _, id := range []string{"k1", "k2", "k3"} {
				rows = append(rows, CriterionVerdict{Criterion: id, Verdict: "met", Kind: CriterionTest})
			}
			if err := g.s.PutCriteriaVerdicts(context.Background(), "t1-int", CriteriaSourceGate, FactoryDecider, rows); err != nil {
				t.Fatal(err)
			}
		}, FactoryDecider, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _, _, c := planCriteriaRig(t, "m1", "m1")
			ctx := context.Background()
			tc.settle(t, g)
			g.r.CriteriaPass(ctx)
			want := &CaseResolution{By: tc.by, Rung: tc.rung, Action: ActAnswerCriteria, Reason: "every criterion is met or waived"}
			if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, want) {
				t.Fatalf("case %s, resolution %+v; want it resolved by %+v", got.Status, got.Resolution, want)
			}
			if a, ok, err := g.s.NewestApproval(ctx, "t1", ApprovalCriteria); err != nil || !ok || a.Status != "approved" || a.DecidedBy != tc.by {
				t.Fatalf("request = %+v, %v, %v; want it approved by %s", a, ok, err, tc.by)
			}
		})
	}
}

// An agent card's stop at the month cap opens month_cap, not the factory's
// month cap: no exception passes it to the human, so it climbs to rung 2,
// which analyses it, with one step in progress, one case_climbed row and
// no face yet.
func TestAnAgentCardsMonthCapStopClimbs(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	out := g.trip(t, monthCapReached+" for 2026-11")
	g.settle()
	if c := caseByID(t, g.s, out.CaseID); c.Kind != CaseMonthCap || c.Status != CaseOpen || out.Rung != 2 {
		t.Fatalf("outcome %+v, case %s %s; want the month cap open with rung 2", out, c.Kind, c.Status)
	}
	if two := g.stepsOfRung(t, out.CaseID, 2); len(two) != 1 || two[0].Outcome != StepInProgress {
		t.Fatalf("rung 2 steps %+v; want one in progress", two)
	}
	if rows := g.rows(t, EventCaseClimbed); len(rows) != 1 || rows[0].Detail["case_id"] != float64(out.CaseID) || len(g.forge.comments) != 0 {
		t.Fatalf("case_climbed rows %+v, comments %d; want one for case %d and no face", rows, len(g.forge.comments), out.CaseID)
	}
}
