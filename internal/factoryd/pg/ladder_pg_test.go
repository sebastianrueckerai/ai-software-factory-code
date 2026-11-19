package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A session cap on an open pull request whose head h2 is green and
// approved: rung 0 routes it to the approval in place. The case reads
// resolved by factoryd on rung 0 with route_approval, its one step is
// rung 0's, no comment is posted and no question is asked. The run hears
// of it.
func TestOpenTripRoutesAGreenApprovedHeadToApproval(t *testing.T) {
	g := newTripRig(t, "success")
	g.atHead(t, true)
	out := g.trip(t, tripCap)

	if out.Resolution == nil || out.Resolution.Action != ActRouteApproval || out.Comment != 0 || out.Rung != 0 || out.CaseID == 0 {
		t.Fatalf("outcome = %+v, want route_approval on rung 0 with no comment", out)
	}
	c := caseByID(t, g.s, out.CaseID)
	if c.Kind != CaseSessionCap || c.StateKey != "h2" || c.Status != CaseResolved ||
		!reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActRouteApproval}) {
		t.Fatalf("case = %+v, resolution %+v; want the session cap at h2 resolved by factoryd on rung 0 with route_approval", c, c.Resolution)
	}
	steps := caseStepsOf(t, g.s, c.ID)
	if len(steps) != 1 || steps[0].Rung != 0 || steps[0].Actor != FactoryDecider || steps[0].Decision != ActRouteApproval || steps[0].Outcome != StepResolved {
		t.Fatalf("steps = %+v, want rung 0's resolving step alone", steps)
	}
	if len(g.forge.comments) != 0 || len(g.asks(t)) != 0 {
		t.Fatalf("comments %q, asks %d; want none", g.forge.comments, len(g.asks(t)))
	}
	if got := g.signals.got; len(got) != 1 || got[0].signal != SignalCaseResolved || got[0].sig.Case == nil || got[0].sig.Case.CaseID != c.ID {
		t.Fatalf("signals = %+v, want case_resolved of case %d", got, c.ID)
	}
}

// A trip opens its case with no wake and runs rung 0 and the climb itself,
// so the case-wake pass does not wake it. The green approved head's trip:
// rung 0 resolves its case, which reads rung 0's with no wake. A stop of
// kind other on the same card: no rule of rung 0 fits and no rung 1 judges
// on a card with no check-in model, so rung 2 takes the case, its wake at
// rung 2's due 35m on. One wake pass now leaves both with the steps their
// trips wrote.
func TestTheWakePassLeavesATripsOwnCase(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	g.atHead(t, true)
	routed := g.trip(t, tripCap)
	if c := caseByID(t, g.s, routed.CaseID); c.Status != CaseResolved || c.OwnerRung != 0 || c.NextWakeAt != nil {
		t.Fatalf("the routed case = %+v, want it resolved, rung 0's, with no wake", c)
	}
	climbed := g.trip(t, "the run stopped on something no rule names")
	before := map[int64][]CaseStep{routed.CaseID: caseStepsOf(t, g.s, routed.CaseID), climbed.CaseID: caseStepsOf(t, g.s, climbed.CaseID)}
	if steps := before[climbed.CaseID]; climbed.Rung != 2 || len(steps) != 2 || steps[0].Rung != 0 || steps[1].Rung != 2 || steps[1].Outcome != StepInProgress {
		t.Fatalf("the trip = %+v, steps %+v; want rung 0's hand-up and rung 2 in progress", climbed, steps)
	}
	v := g.r.PgCfg().Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: time.Now}
	if err := m.WakeCases(ctx); err != nil {
		t.Fatalf("WakeCases: %v", err)
	}
	for id, want := range before {
		if got := caseStepsOf(t, g.s, id); !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d's steps after the wake pass = %+v, want the trip's %+v", id, got, want)
		}
	}
}

// A repository that names no project has no judgment budget to read: a
// failed dependency's trip, which rung 1 never takes, gets no rung 2 step
// either, and faces with the problem and the ask as the case's own and the
// agent's view saying why no rung judged it, rung 0's step marked "not
// judged, judgment budget unreadable". With no project the card's
// configuration is unread, so rung 0 takes nothing.
func TestATripOfNoProjectFacesMarkedUnreadable(t *testing.T) {
	g := newTripRig(t, "success")
	v := g.r.PgCfg().Policy()
	v.Projects = maps.Clone(v.Projects)
	delete(v.Projects, "demo-app")
	g.r.PgSetPolicy(func() policy.Version { return v })
	reason := DepFailedReason(Dependency{Issue: 7, TaskID: "t-a", Status: "failed"})
	out := g.trip(t, reason)
	const seen = "The factory agent did not judge it: demo-app/demo-app names no project, so the factory cannot read its judgment budget."
	c := caseByID(t, g.s, out.CaseID)
	if out.Rung != 3 || c.Kind != CaseDependencyFailed || c.Status != CaseAtHuman || strings.Contains(c.Problem, "not judged") {
		t.Fatalf("outcome %+v, case %s %s %q; want the failed dependency at the human, its problem its own", out, c.Kind, c.Status, c.Problem)
	}
	asks := g.asks(t)
	if len(asks) != 1 || len(g.forge.comments) != 1 {
		t.Fatalf("asks %+v, comments %d; want one face", asks, len(g.forge.comments))
	}
	info, _ := asks[0].Detail["info"].(string)
	if ask, _ := asks[0].Detail["ask"].(string); strings.Contains(ask, "not judged") || !strings.Contains(info, seen) {
		t.Fatalf("the face's ask %q, info %q; want the ask as the case's own and the view %q", ask, info, seen)
	}
	var found []string
	for _, st := range caseStepsOf(t, g.s, c.ID) {
		found = append(found, fmt.Sprintf("%d %s", st.Rung, st.Found))
	}
	want := []string{"0 facts read partly: config, so rung 0 took nothing", "0 not judged, judgment budget unreadable: repository demo-app/demo-app names no project, so its judgment budget cannot be read", "3 "}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("steps %q, want %q", found, want)
	}
}

// A red head's checks run again once: the first trip resolves with
// rerun_checks, after which the facts read RerunOnHead. The same stop on
// the same head opens the case that follows, rung 0 hands it up, rung 2
// hands it on, and the human is asked: one comment in all.
func TestOpenTripRerunsRedChecksOnceThenAsks(t *testing.T) {
	g := newTripRig(t, "failure")
	rung1Off(g.r)
	g.atHead(t, false)
	first := g.trip(t, tripCap)
	if first.Resolution == nil || first.Resolution.Action != ActRerunChecks || first.Comment != 0 {
		t.Fatalf("first outcome = %+v, want rerun_checks with no comment", first)
	}
	card, _, _ := g.s.Task(context.Background(), "t1")
	if f, err := g.r.PgTripFacts(context.Background(), card, "", ""); err != nil || !f.RerunOnHead || f.Checks != "red" {
		t.Fatalf("facts after = %+v, %v; want the checks of red h2 run again", f, err)
	}

	second := g.faced(t, tripCap)
	if second.Resolution != nil || second.CaseID == first.CaseID || second.Comment != 501 || second.Rung != 3 {
		t.Fatalf("second outcome = %+v, want a new case at the human on comment 501", second)
	}
	c := caseByID(t, g.s, second.CaseID)
	if c.Status != CaseAtHuman || c.StateKey != "h2" || c.Follows != first.CaseID || c.CommentID != 501 {
		t.Fatalf("second case = %+v, want h2's at the human following case %d with comment 501", c, first.CaseID)
	}
	steps := caseStepsOf(t, g.s, c.ID)
	if len(steps) != 3 || steps[0].Rung != 0 || steps[0].Outcome != StepHandedUp ||
		steps[0].Found != "rung 0 took this stop with rerun_checks before, and it came back" || steps[1].Rung != 2 || steps[1].Outcome != StepHandedUp ||
		steps[2].Rung != 3 || steps[2].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0 handing up the stop that came back, rung 2's hand-up, then the human's", steps)
	}
	if len(g.forge.comments) != 1 || len(g.asks(t)) != 1 {
		t.Fatalf("comments %d, asks %d; want the human face once", len(g.forge.comments), len(g.asks(t)))
	}
}

// Two trips with one key while the case waits at the human: one case,
// whose second step says the stop came again, and one comment.
func TestOpenTripRepeatsAppendAndAskNothingNew(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	g.atHead(t, false)
	first := g.faced(t, tripCap)
	again := g.trip(t, tripCap)

	if again != first || first.Comment != 501 || first.Rung != 3 {
		t.Fatalf("outcomes %+v and %+v, want the same case on comment 501 at the human", first, again)
	}
	if cases, _ := g.s.CasesOf(context.Background(), "t1"); len(cases) != 1 {
		t.Fatalf("cases = %+v, want one", cases)
	}
	c := caseByID(t, g.s, first.CaseID)
	var found []string
	for _, st := range caseStepsOf(t, g.s, c.ID) {
		found = append(found, fmt.Sprintf("%d %s %s", st.Rung, st.Outcome, st.Found))
	}
	want := []string{"0 handed_up no resolver fits", "2 handed_up ", "3 in_progress ", "0 handed_up the same stop again: " + c.Problem}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("steps %q, want %q", found, want)
	}
	if len(g.forge.comments) != 1 || len(g.asks(t)) != 1 {
		t.Fatalf("comments %d, asks %d; want one each", len(g.forge.comments), len(g.asks(t)))
	}
}

// The money and cap asks of 2026-11-14 fired bare while unresolved
// review findings stood on the head (#1879 step 36, #1889 step 45,
// #1899 case 3826). A cap ask whose head holds unresolved review or
// audit findings counts them: the case's problem and the ask the human
// answers say how many stand, and name none of them. The same stop on a
// head whose findings a newer review resolved asks the bare cap ask.
func TestACapAskCountsTheFindingsStandingOnTheHead(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rung1Off(g.r)
	g.atHead(t, false)
	tl := `{"step":"model_review","time":"2026-11-14T10:00:00Z","commit":"h2","verdict":"concerns","findings":["the flag is read where it is not checked"]}` + "\n" +
		`{"step":"audit","time":"2026-11-14T10:01:00Z","commit":"h2","verdict":"findings","findings":[{"item":"the migration runs outside its transaction"}]}` + "\n"
	if err := g.r.PgObjects().Put(ctx, "sessions/t1/timeline.jsonl", []byte(tl)); err != nil {
		t.Fatal(err)
	}
	out := g.faced(t, tripCap)
	if out.Resolution != nil || out.Comment != 501 || out.Rung != 3 {
		t.Fatalf("outcome = %+v, want the case at the human on comment 501", out)
	}
	c := caseByID(t, g.s, out.CaseID)
	asks := g.asks(t)
	if len(asks) != 1 {
		t.Fatalf("asks = %d, want one", len(asks))
	}
	ask, _ := asks[0].Detail["ask"].(string)
	if c.Kind != CaseSessionCap || c.Problem != ask || !strings.HasPrefix(ask, "The card used all") || !strings.Contains(ask, " with 2 findings open.") {
		t.Fatalf("case %q = %q, ask %q; want the session cap counting the 2 findings of h2", c.Kind, c.Problem, ask)
	}
	for _, text := range []string{"the flag is read where it is not checked", "the migration runs outside its transaction"} {
		if strings.Contains(ask, text) {
			t.Fatalf("ask = %q names the finding %q", ask, text)
		}
	}

	// the same stop on a head whose findings a newer review resolved: the
	// bare cap ask
	g2 := newTripRig(t, "success")
	rung1Off(g2.r)
	g2.atHead(t, false)
	resolved := `{"step":"model_review","time":"2026-11-14T10:00:00Z","commit":"h2","verdict":"concerns","findings":["the flag is read where it is not checked"]}` + "\n" +
		`{"step":"model_review","time":"2026-11-14T11:00:00Z","commit":"h2","verdict":"concerns"}` + "\n"
	if err := g2.r.PgObjects().Put(ctx, "sessions/t1/timeline.jsonl", []byte(resolved)); err != nil {
		t.Fatal(err)
	}
	out2 := g2.faced(t, tripCap)
	if out2.Resolution != nil || out2.Comment != 501 || out2.Rung != 3 {
		t.Fatalf("outcome = %+v, want the case at the human on comment 501", out2)
	}
	asks2 := g2.asks(t)
	if len(asks2) != 1 {
		t.Fatalf("asks = %d, want one", len(asks2))
	}
	bare, _ := asks2[0].Detail["ask"].(string)
	if !strings.HasPrefix(bare, "The card used all") || strings.Contains(bare, "finding") {
		t.Fatalf("ask = %q, want the bare session cap ask", bare)
	}
}

// Card #2110's shape: an agent-time cap on an open pull request whose head
// h2 is green and approved, after Forgejo refused to merge main into h2 at
// the merge queue's turn. Rung 0 routes nothing to the approval: its step
// finds no resolver, and the case reaches the human with its facts holding
// the conflict and its face naming it in what is left. On those facts rung
// 1 is offered extend, and neither route_approval nor reverify.
func TestOpenTripAtAMergeConflictRoutesNothingToTheApproval(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rung1Off(g.r)
	g.atHead(t, true)
	refusal := "the branch conflicts with its base: status 409: merge failed because of conflict"
	if err := g.r.PgTimeline().Append(ctx, "t1", "merge_conflict", map[string]any{"pr": 9, "commit": "h2", "refusal": refusal}); err != nil {
		t.Fatal(err)
	}
	out := g.faced(t, TimeCapReachedLadder+"\nlast: the pull request no longer merges into main")
	if out.Resolution != nil || out.Comment != 501 || out.Rung != 3 {
		t.Fatalf("outcome = %+v, want the case at the human on comment 501", out)
	}
	c := caseByID(t, g.s, out.CaseID)
	const left = "No finding is open, and the checks are green. The pull request no longer merges into main. Only a session that merges main into its branch fixes that."
	if c.Kind != CaseAgentTimeCap || c.Facts["merge_conflict"] != refusal {
		t.Fatalf("case %s, conflict %v; want the agent-time cap holding the conflict", c.Kind, c.Facts["merge_conflict"])
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) != 3 || steps[0].Rung != 0 || steps[0].Found != "no resolver fits" || steps[1].Rung != 2 {
		t.Fatalf("steps = %+v, want rung 0 finding no resolver, rung 2's hand-up, then the human's", steps)
	}
	asks := g.asks(t)
	if len(asks) != 1 {
		t.Fatalf("asks = %d, want one", len(asks))
	}
	if story, _ := asks[0].Detail["story"].(map[string]any); story["Left"] != left {
		t.Fatalf("the face's story = %v, want what is left to say %q", asks[0].Detail["story"], left)
	}
	card, _, _ := g.s.Task(ctx, "t1")
	f, err := g.r.FactsAtCase(ctx, card, c)
	if err != nil {
		t.Fatal(err)
	}
	offered := offeredNames(f, 1)
	if slices.Contains(offered, ActRouteApproval) || slices.Contains(offered, ActReverify) || !slices.Contains(offered, ActExtend) {
		t.Fatalf("rung 1 is offered %v, want extend and no gate rerun", offered)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A check-in
// stop's trip carries its mark into its case's facts, and the card's cases
// give it back to a new run: the newest one kept, past a later stop of
// another kind that keeps none.
func TestOpenTripKeepsTheCheckinStopsMark(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rung1Off(g.r)
	mark := CheckinStopMark{Kind: CheckGoalConflict, Tip: "h2", ExtraLines: 40, RaisedSessions: 1}
	for _, in := range []TripInput{
		{TaskID: "t1", Reason: "check-in: goal_conflict: the objective contradicts the review", Extra: map[string]any{FactCheckinMark: CheckinStopMark{Kind: CheckSpend, Tip: "h0"}}},
		{TaskID: "t1", Reason: "check-in: goal_conflict: the review asks for what the spec forbids", Head: "h2", Extra: map[string]any{FactCheckinMark: mark}},
		{TaskID: "t1", Reason: tripCap},
	} {
		if _, err := g.r.OpenTrip(ctx, in, tripAttempt{N: 1}); err != nil {
			t.Fatalf("OpenTrip(%q): %v", in.Reason, err)
		}
	}
	cases, err := g.s.CasesOf(ctx, "t1")
	if err != nil || len(cases) != 3 {
		t.Fatalf("cases = %+v, %v; want three", cases, err)
	}
	if got := CheckinMarkOf(cases); got == nil || *got != mark {
		t.Fatalf("the kept mark = %+v, want %+v", got, mark)
	}
}

// A restart's trip waits on the card's live ask with a step and no new
// face. With no live ask, the stop the card asked last, the agent's ask,
// opens its own case again and the run waits on it.
func TestOpenTripHealedFindsTheLiveAsk(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rung1Off(g.r)
	asked := g.faced(t, AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt")
	healed := g.trip(t, HealedReason)
	if healed != asked || len(g.forge.comments) != 1 {
		t.Fatalf("healed %+v after %+v with %d comments; want the same case and comment, no new face", healed, asked, len(g.forge.comments))
	}
	steps := caseStepsOf(t, g.s, asked.CaseID)
	if last := steps[len(steps)-1]; last.Rung != 0 || last.Outcome != StepHandedUp || last.Found != "asked again after a restart" {
		t.Fatalf("last step = %+v, want the restart's", last)
	}

	if err := g.s.WithdrawCase(ctx, asked.CaseID, "moved on"); err != nil {
		t.Fatal(err)
	}
	again := g.trip(t, HealedReason)
	c := caseByID(t, g.s, again.CaseID)
	if again.Comment != 502 || c.Kind != CaseAgentEscalate || c.Status != CaseAtHuman || c.Problem != "Which file?" || c.CommentID != 502 {
		t.Fatalf("outcome %+v, case %+v; want the question asked again on comment 502 as the agent's ask", again, c)
	}
}

// A report card's restart waits on its stop's case at the human as a work
// card's does: the card has no issue, so the gate stop's face is on the card
// alone, comment 0, and the healed trip names the same case with a step of
// rung 0 and no new face, ask or case. The card keeps waiting-human.
func TestOpenTripHealedOnAReportCardFindsItsStop(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	rung1Off(g.r)
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET source = $1, issue_number = 0 WHERE id = 't1'`, SourceSizing); err != nil {
		t.Fatal(err)
	}
	asked := g.faced(t, GateStopped+"RunReport (attempt 1) past its bound of 10m0s: the session made no call\nRecommendation: run it again")
	if c := caseByID(t, g.s, asked.CaseID); asked.Comment != 0 || asked.Rung != 3 || c.Kind != CaseGateStop || c.Status != CaseAtHuman || len(g.forge.comments) != 0 {
		t.Fatalf("asked %+v, case %+v, %d comments; want the gate stop at the human with no comment", asked, c, len(g.forge.comments))
	}
	healed := g.trip(t, HealedReason)
	cases, err := g.s.CasesOf(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	task, _, _ := g.s.Task(ctx, "t1")
	if healed != asked || len(cases) != 1 || len(g.asks(t)) != 1 || len(g.forge.comments) != 0 || task.Status != "waiting-human" {
		t.Fatalf("healed %+v after %+v: %d cases, %d asks, %d comments, status %s; want the same case, one case and ask, no comment, waiting-human",
			healed, asked, len(cases), len(g.asks(t)), len(g.forge.comments), task.Status)
	}
	steps := caseStepsOf(t, g.s, asked.CaseID)
	if last := steps[len(steps)-1]; last.Rung != 0 || last.Outcome != StepHandedUp || last.Found != "asked again after a restart" {
		t.Fatalf("last step = %+v, want the restart's", last)
	}
}

// The human face's question_asked row names its case, and the case writer
// appends the human's step to that case and takes its comment, opening no
// case of the ask's words.
func TestOpenTripWritesTheFaceWithTheCaseID(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	out := g.faced(t, AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt")

	asks := g.asks(t)
	if len(asks) != 1 || asks[0].Detail["case_id"] != float64(out.CaseID) || asks[0].Detail["comment"] != 501.0 {
		t.Fatalf("asks = %+v, want one naming case %d and comment 501", asks, out.CaseID)
	}
	cases, err := g.s.CasesOf(context.Background(), "t1")
	if err != nil || len(cases) != 1 || cases[0].ID != out.CaseID {
		t.Fatalf("cases = %+v, %v; want the trip's alone", cases, err)
	}
	c := cases[0]
	if c.Kind != CaseAgentEscalate || c.Status != CaseAtHuman || c.CommentID != 501 || c.Problem != "Which file?" || c.Facts["source"] != "agent" {
		t.Fatalf("case = %+v, want the agent's ask at the human on comment 501", c)
	}
	if steps := caseStepsOf(t, g.s, c.ID); len(steps) != 3 || steps[1].Rung != 2 || steps[2].Rung != 3 || steps[2].Actor != FactoryDecider || steps[2].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0's, rung 2's, then the human's", steps)
	}
}

// A run from before the cases gate asks through Escalate: the stop's case
// opens at the human by the trip's key, the head, and the face names it.
func TestEscalateOpensTheCaseByTheTripsKey(t *testing.T) {
	g := newTripRig(t, "success")
	g.atHead(t, true)
	id, err := g.r.FaceStop(context.Background(), "t1", tripCap, nil)
	if err != nil || id != 501 {
		t.Fatalf("Escalate = %d, %v; want comment 501", id, err)
	}
	cases, _ := g.s.CasesOf(context.Background(), "t1")
	if len(cases) != 1 || cases[0].Kind != CaseSessionCap || cases[0].StateKey != "h2" || cases[0].Status != CaseAtHuman || cases[0].CommentID != 501 {
		t.Fatalf("cases = %+v, want the session cap at h2 at the human on comment 501, no rung 0", cases)
	}
	if asks := g.asks(t); len(asks) != 1 || asks[0].Detail["case_id"] != float64(cases[0].ID) {
		t.Fatalf("asks = %+v, want one naming case %d", asks, cases[0].ID)
	}
}

// Escalate opens its case open and puts it at the human once its face is
// written, as OpenTrip does: a face whose comment fails leaves the case
// open, faceless, for the heal pass to weigh by its age.
func TestEscalateOpensItsCaseBeforeItsFace(t *testing.T) {
	g := newTripRig(t, "success")
	g.forge.failComment = true
	if _, err := g.r.FaceStop(context.Background(), "t1", tripCap, nil); err == nil {
		t.Fatal("Escalate = nil; want the comment's failure")
	}
	cases, err := g.s.CasesOf(context.Background(), "t1")
	if err != nil || len(cases) != 1 || cases[0].Kind != CaseSessionCap || cases[0].Status != CaseOpen {
		t.Fatalf("cases = %+v, %v; want the session cap open, its face not written", cases, err)
	}
}

// A budget floor's case carries the money: 0.30 EUR an opening needs less
// what a session may spend, the 0.15 left of 3 EUR with 2.85 spent less
// the 0.40 gate reserve, -0.25, is 0.55 EUR, which buys one session on
// the card's model. The issue comment names that amount under the question
// and beside the answer that adds it.
func TestOpenTripCarriesTheMoneyFact(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	if _, err := g.s.PgDB().ExecContext(context.Background(), `UPDATE tasks SET spent_eur = 2.85, model_alias = 'factory/small' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out := g.faced(t, BudgetRefused+": 0.150 EUR of the 3.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR")

	c := caseByID(t, g.s, out.CaseID)
	want := CaseMoney{Kind: "budget", BudgetEUR: 3, SpentEUR: 2.85, Buys: "one session on factory/small"}
	if c.Kind != CaseBudgetFloor || c.Money == nil || *c.Money != want || out.Comment != 501 {
		t.Fatalf("case %+v, money %+v; want the budget floor at the human with %+v", c, c.Money, want)
	}
	if len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], "and the card needs 0.55 EUR more to pay its next session.") ||
		!strings.Contains(g.forge.comments[0], "Press “Add money” (EUR to add: 0.55), “End the card” or “Not now” on the board") {
		t.Fatalf("comments %q, want the face naming the 0.55 EUR to add", g.forge.comments)
	}
}

// An ask that names its live case puts that case at the human with the
// ask's comment and the human's step. One that names a case that ended
// opens a case of its words, as an ask that names none does.
func TestAnAskNamingItsCaseTakesThatCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	c, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseSessionCap, StateKey: "h2", Problem: "Card 1410 used its sessions"})
	if err != nil {
		t.Fatal(err)
	}
	ask := map[string]any{"question": capAsk, "ask": capAsk, "comment": int64(27001), "source": "cap", "reason": CapReached, "case_id": c.ID}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, ask)
	got := onlyOpenCase(t, s)
	if got.ID != c.ID || got.Status != CaseAtHuman || got.CommentID != 27001 || got.Problem != "Card 1410 used its sessions" {
		t.Fatalf("case = %+v, want case %d at the human on comment 27001", got, c.ID)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Rung != 3 || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want the human's", steps)
	}

	if err := s.WithdrawCase(ctx, c.ID, "moved on"); err != nil {
		t.Fatal(err)
	}
	ask["comment"] = int64(27002)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, ask)
	if again := onlyOpenCase(t, s); again.ID == c.ID || again.Problem != capAsk || again.CommentID != 27002 || again.Follows != c.ID {
		t.Fatalf("case = %+v, want a case of the ask's words following case %d", again, c.ID)
	}
}

// h1 reached its approval green and approved, then a change request sent
// the card back, and its sessions pushed h2, whose checks are red. The
// session cap's trip on h2 decides on h2: its checks run again, no route
// to the approval on h1's facts.
func TestOpenTripDecidesOnTheTripsHead(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "failure")
	g.forge.checks["h1"] = "success"
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{
		{Kind: EventGateHead, Detail: map[string]any{"gate": GateMerge, "head": "h1"}},
		{Kind: EventModelReview, Detail: map[string]any{"commit": "h1", "round": 1, "reviewed": true, "verdict": "approve"}},
	} {
		e.Repository, e.TaskID, e.IssueNumber, e.Actor, e.Via = "demo-app/demo-app", "t1", 12, FactoryDecider, ViaFactoryd
		if _, err := g.s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	out, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap, Head: "h2"}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolution == nil || out.Resolution.Action != ActRerunChecks {
		t.Fatalf("outcome = %+v, want the checks of h2 run again", out)
	}
	c := caseByID(t, g.s, out.CaseID)
	if c.StateKey != "h2" || c.Facts["head"] != "h2" || c.Facts["checks"] != "red" || c.Facts["review_approved"] != nil {
		t.Fatalf("case key %q, facts %v; want h2's red checks and no approval", c.StateKey, c.Facts)
	}
}

// A head a rerun made and a head a rerun ran on both count as run again:
// the queue ran h2's checks again as h3. A trip on red h3 runs no checks
// again, and the human is asked.
func TestTripFactsCountAHeadARerunMade(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "failure")
	rung1Off(g.r)
	g.forge.checks["h3"] = "failure"
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.r.PgRecord(ctx, "t1", "checks_rerun", map[string]any{"commit": "h2", "head": "h3"})
	card, _, _ := g.s.Task(ctx, "t1")
	for head, want := range map[string]bool{"h2": true, "h3": true, "h4": false} {
		if f, err := g.r.PgTripFacts(ctx, card, head, ""); err != nil || f.RerunOnHead != want || f.Head != head {
			t.Errorf("facts on %s: head %q, RerunOnHead %v, %v; want %v", head, f.Head, f.RerunOnHead, err, want)
		}
	}
	out, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t1", Reason: tripCap, Head: "h3"}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	out = g.rung2Faces(t, out)
	steps := caseStepsOf(t, g.s, out.CaseID)
	if out.Resolution != nil || out.Comment != 501 || len(steps) == 0 || steps[0].Found != "no resolver fits" {
		t.Fatalf("outcome %+v, steps %+v; want the human asked with no resolver fitting", out, steps)
	}
}

// An attempt of the trip retried after its first handed the case to rung 2
// writes no step again and starts no second rung 2; one retried after rung
// 2's hand-up faced the case writes nothing more and asks nothing new.
func TestOpenTripRetriedWritesItsStepsOnce(t *testing.T) {
	ctx := context.Background()
	steps := func(g tripRig, id int64) []string {
		var out []string
		for _, st := range caseStepsOf(t, g.s, id) {
			out = append(out, fmt.Sprintf("%d %s %s", st.Rung, st.Outcome, st.Found))
		}
		return out
	}
	t.Run("after the climb", func(t *testing.T) {
		g := newTripRig(t, "success")
		rung1Off(g.r)
		in := TripInput{TaskID: "t1", Reason: tripCap}
		first, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 1})
		if err != nil || first.Rung != 2 {
			t.Fatalf("first attempt = %+v, %v; want rung 2 holding the case", first, err)
		}
		again, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 2})
		if err != nil || again != first {
			t.Fatalf("second attempt = %+v, %v; want %+v", again, err, first)
		}
		if got, want := steps(g, first.CaseID), []string{"0 handed_up no resolver fits", "2 in_progress "}; !reflect.DeepEqual(got, want) || len(actRig{s: g.s}.rows(t, EventCaseClimbed)) != 1 {
			t.Fatalf("steps %q, case_climbed rows %d; want %q and one row", got, len(actRig{s: g.s}.rows(t, EventCaseClimbed)), want)
		}
	})
	t.Run("after the face", func(t *testing.T) {
		g := newTripRig(t, "success")
		rung1Off(g.r)
		in := TripInput{TaskID: "t1", Reason: tripCap}
		first, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 1})
		if err != nil {
			t.Fatal(err)
		}
		first = g.rung2Faces(t, first)
		want := []string{"0 handed_up no resolver fits", "2 handed_up ", "3 in_progress "}
		again, err := g.r.OpenTrip(ctx, in, tripAttempt{ID: "run/a", N: 2})
		if err != nil || again != first {
			t.Fatalf("second attempt = %+v, %v; want %+v", again, err, first)
		}
		if got := steps(g, first.CaseID); !reflect.DeepEqual(got, want) || len(g.forge.comments) != 1 {
			t.Fatalf("steps %q, comments %d; want %q and one comment", got, len(g.forge.comments), want)
		}
	})
}

// An ask that names another card's case goes by its words: a case of the
// asking card opens, and the other card's case is left as it was.
func TestAnAskNamingAnotherCardsCaseGoesByItsWords(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	if err := s.CreateTask(ctx, Task{ID: "t-other", Kind: "agent", Status: "waiting-human", Objective: "y", Repository: "sf/r",
		IssueNumber: 1411, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatal(err)
	}
	other, _, err := s.OpenCase(ctx, Case{TaskID: "t-other", Repository: "sf/r", Kind: CaseSessionCap, StateKey: "h2", Problem: "Card 1411 used its sessions"})
	if err != nil {
		t.Fatal(err)
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": capAsk, "ask": capAsk, "comment": int64(27001), "source": "cap", "reason": CapReached, "case_id": other.ID})
	mine := onlyOpenCase(t, s)
	if mine.ID == other.ID || mine.Problem != capAsk || mine.CommentID != 27001 {
		t.Fatalf("the asking card's case = %+v, want one of its own words", mine)
	}
	if got := caseByID(t, s, other.ID); got.Status != CaseOpen || got.CommentID != 0 || len(caseStepsOf(t, s, other.ID)) != 0 {
		t.Fatalf("the other card's case = %+v, want it untouched", got)
	}
}

// extendOne is sebastian's extend of one session on t1's case id, carried
// by his comment 77, and what it left: the card's raised sessions and its
// card_extended rows.
func (g tripRig) extendOne(t *testing.T, id int64) (int, []Event) {
	t.Helper()
	if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: id, By: "sebastian", Via: ViaConsole, Rung: 3,
		Post: posting(77, nil), Res: oneSession}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	card, _, err := g.s.Task(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return card.Usage.RaisedSessions, actRig{s: g.s}.rows(t, EventCardExtended)
}

// A run from before the cases gate asks through Escalate and grants its
// own session when the human's comment wakes it: the human's extend on
// that case writes its draw against the pool as a row of rung 3 and
// raises nothing on the card's row. A case a trip under the gate waits on
// carries the trip's step of rung 0, and the extend raises the row there:
// the case the trip opened, and the old run's case a restarted run under
// the gate waits on.
func TestActLeavesTheGrantToARunFromBeforeTheGate(t *testing.T) {
	poolRow := func(t *testing.T, rows []Event) {
		t.Helper()
		if len(rows) != 1 || rows[0].Detail["pool"] != true || rows[0].Detail["rung"] != 3.0 || rows[0].Detail["sessions"] != 1.0 {
			t.Fatalf("card_extended rows = %+v, want one pool draw of 1 session on rung 3", rows)
		}
	}
	t.Run("a run from before the gate", func(t *testing.T) {
		g := newTripRig(t, "success")
		if _, err := g.r.FaceStop(context.Background(), "t1", tripCap, nil); err != nil {
			t.Fatal(err)
		}
		c := onlyLiveCase(t, g.s, "t1")
		raised, rows := g.extendOne(t, c.ID)
		poolRow(t, rows)
		if raised != 0 {
			t.Fatalf("raised sessions = %d, want 0: the run grants its own", raised)
		}
	})
	for name, open := range map[string]func(t *testing.T, g tripRig) int64{
		"a trip's case": func(t *testing.T, g tripRig) int64 { return g.trip(t, tripCap).CaseID },
		"the old run's case after a restart": func(t *testing.T, g tripRig) int64 {
			if _, err := g.r.FaceStop(context.Background(), "t1", tripCap, nil); err != nil {
				t.Fatal(err)
			}
			return g.trip(t, HealedReason).CaseID
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newTripRig(t, "success")
			id := open(t, g)
			if c := onlyLiveCase(t, g.s, "t1"); c.ID != id {
				t.Fatalf("live case %d, the trip waits on %d", c.ID, id)
			}
			raised, rows := g.extendOne(t, id)
			poolRow(t, rows)
			if raised != 1 {
				t.Fatalf("raised sessions = %d, want 1: the run under the gate reads the row", raised)
			}
		})
	}
}

// onlyLiveCase is taskID's one live case.
func onlyLiveCase(t *testing.T, s *PGStore, taskID string) Case {
	t.Helper()
	open, err := s.OpenCasesOf(context.Background(), taskID)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenCasesOf = %+v, %v; want one case", open, err)
	}
	return open[0]
}

// The face's last line says what moves the card on, by the case's kind:
// under the cases gate a reply at a cap or at a budget floor is a continue
// that asks again, so the face names every answer the card holds and the
// chat; any other stop takes a reply. A run from before the gate grants
// on a reply, and its face says so at a cap too.
func TestOpenTripsFaceNamesWhatMovesTheCard(t *testing.T) {
	const reply = "Reply on this issue to start a new attempt"
	const chat = " on the board, or tell the project chat how the card goes on. A reply alone starts no attempt."
	extend := "Press “Allow more sessions”, “End the card” or “Not now”" + chat
	// the rig's card has its whole budget left: the face holds no money answer
	fund := "Press “End the card” or “Not now”" + chat
	for _, tc := range []struct {
		name, reason, want string
		escalate           bool
	}{
		{"the session cap", tripCap, extend, false},
		{"the agent time cap", TimeCapReached + "\nlast: no session produced a result", extend, false},
		{"the budget floor", BudgetRefused + ": 0.150 EUR of the 3.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR", fund, false},
		{"an agent's question", AgentAsks + "Which file?\nWhy: two match\nRecommendation: a.txt", reply, false},
		{"a run from before the gate at the session cap", tripCap, reply, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTripRig(t, "success")
			rung1Off(g.r)
			if tc.escalate {
				if _, err := g.r.FaceStop(context.Background(), "t1", tc.reason, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				g.faced(t, tc.reason)
			}
			if len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], tc.want) {
				t.Fatalf("faces = %q, want one ending %q", g.forge.comments, tc.want)
			}
		})
	}
}

// A run under the gate that restarts while its card waits on a human, with
// no live case left, asks its stop again as the stop's own case: the
// newest escalation, the session cap's, opens the cap's case with a step of
// rung 0 that says so, and the face names Extend, not a reply. The
// human's extend there raises the card's row, which that run reads.
func TestOpenTripRestartWithNoLiveCaseAsksItsStopAgain(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	first := g.faced(t, tripCap)
	if err := g.s.WithdrawCase(context.Background(), first.CaseID, "the run that asked it is gone"); err != nil {
		t.Fatal(err)
	}
	g.forge.comments = nil
	out := g.trip(t, HealedReason)
	c := caseByID(t, g.s, out.CaseID)
	if c.Kind != CaseSessionCap || c.Status != CaseAtHuman || out.Comment != 501 || c.CommentID != 501 {
		t.Fatalf("outcome %+v, case %+v; want the session cap's case at the human on comment 501", out, c)
	}
	if !slices.ContainsFunc(caseStepsOf(t, g.s, c.ID), func(s CaseStep) bool { return s.Rung == 0 && s.Found == "asked again after a restart" }) {
		t.Fatalf("steps = %+v, want the restart's step of rung 0", caseStepsOf(t, g.s, c.ID))
	}
	if extend := "Press “Allow more sessions”, “End the card” or “Not now” on the board, or tell the project chat how the card goes on"; len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], extend) {
		t.Fatalf("faces = %q, want one naming Extend", g.forge.comments)
	}
	if raised, _ := g.extendOne(t, c.ID); raised != 1 {
		t.Fatalf("raised sessions = %d, want 1: the run under the gate reads the row", raised)
	}
}

// A restart that finds the card's live ask with no face yet, as a rung 2
// hand-up whose comment failed left it open, gives that case its face
// rather than waiting on a case nobody was asked.
func TestOpenTripRestartFacesALiveAskWithNone(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	g.failedHandUp(t, tripCap)
	open := onlyLiveCase(t, g.s, "t1")
	out := g.trip(t, HealedReason)
	if c := caseByID(t, g.s, open.ID); out.CaseID != open.ID || out.Comment != 501 || c.Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("outcome %+v, case %+v, faces %d; want case %d at the human on comment 501", out, c, len(g.forge.comments), open.ID)
	}
}

// The face a restart gives a live ask with none yet ends on that case's
// own way out: the agent's ask left open by a failed comment takes a
// reply, though the card's newest ask is the merge queue's.
func TestOpenTripRestartFacesALiveAskByItsOwnReason(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	ctx := context.Background()
	g.failedHandUp(t, AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt")
	// the merge queue's ask, faced at comment 400 and withdrawn since, is the card's newest
	if _, err := g.s.Record(ctx, Event{Repository: "sf/r", TaskID: "t1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Put it back in the queue?", "ask": "Put it back in the queue?", "comment": 400, "reason": leftQueue + ": the job is red"}}); err != nil {
		t.Fatal(err)
	}
	queue, err := g.s.NewestAskCase(ctx, "t1")
	if err != nil || queue.Facts["reason"] == AgentAsks+"Which file?\nWhy: two match\nRecommendation: a.txt" {
		t.Fatalf("the queue's case = %+v, %v; want a case of its own", queue, err)
	}
	if err := g.s.WithdrawCase(ctx, queue.ID, "the queue was left"); err != nil {
		t.Fatal(err)
	}
	g.trip(t, HealedReason)
	const reply = "Reply on this issue to start a new attempt with your comment as guidance"
	if len(g.forge.comments) != 1 || !strings.Contains(g.forge.comments[0], reply) {
		t.Fatalf("faces = %q, want one ending %q", g.forge.comments, reply)
	}
}

// failedHandUp is the trip of reason, which rung 2 takes, and rung 2's
// hand-up of its case, whose face's comment fails: the case stays open with
// no face.
func (g tripRig) failedHandUp(t *testing.T, reason string) {
	t.Helper()
	out := g.trip(t, reason)
	g.forge.failComment = true
	if _, err := g.r.HandUpRung2(context.Background(), "t1", out.CaseID, "", RungTwoRecommendation{}, nil); err == nil {
		t.Fatal("HandUpRung2 = nil; want the comment's failure")
	}
}

const tripChecksWait = GateStopped + "WaitChecks (attempt 1) past its bound of 50m: no check reported in the bound."

// A checks-wait stop whose checks are all green, #1652's shape, with the
// head's review approving it too: rung 0 resolves it with wait_checks (not
// route_approval) through Act, no comment, and the run hears of it. The
// same stop on the same head, once the wait ran, goes to the human.
func TestOpenTripWaitsOnGreenChecksOnceThenAsks(t *testing.T) {
	g := newTripRig(t, "success")
	rung1Off(g.r)
	g.atHead(t, true)
	first := g.trip(t, tripChecksWait)
	if first.Resolution == nil || first.Resolution.Action != ActWaitChecks || first.Comment != 0 || first.Rung != 0 || first.CaseID == 0 {
		t.Fatalf("first outcome = %+v, want wait_checks on rung 0 with no comment", first)
	}
	c := caseByID(t, g.s, first.CaseID)
	if c.Kind != CaseGateStop || c.Facts["gate"] != ActivityWaitChecks || c.Status != CaseResolved ||
		!reflect.DeepEqual(c.Resolution, &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWaitChecks}) {
		t.Fatalf("case = %+v, resolution %+v; want the checks-wait stop resolved by factoryd on rung 0 with wait_checks", c, c.Resolution)
	}
	if got := g.signals.got; len(got) != 1 || got[0].signal != SignalCaseResolved || got[0].sig.Case == nil || got[0].sig.Case.CaseID != c.ID {
		t.Fatalf("signals = %+v, want case_resolved of case %d", got, c.ID)
	}
	card, _, _ := g.s.Task(context.Background(), "t1")
	if f, err := g.r.PgTripFacts(context.Background(), card, "", ""); err != nil || !f.WaitedOnHead {
		t.Fatalf("facts after = %+v, %v; want h2 waited on", f, err)
	}
	second := g.faced(t, tripChecksWait)
	if second.Resolution != nil || second.CaseID == first.CaseID || second.Rung != 3 || second.Comment == 0 {
		t.Fatalf("second outcome = %+v, want a new case at the human", second)
	}
}

// A restart asks the question again with no face of its own, as production
// writes it: the newest ask of the case is a whole face all the same, and
// the case's Why stays on its own reason.
func TestARepeatedAskCarriesTheFaceItRepeats(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	reason := CapReached + "\nlast: no session produced a result\nRecommendation: a.txt, it is first"
	if _, err := g.r.FaceStop(ctx, "t1", reason, []string{"a.txt", "b.txt"}); err != nil {
		t.Fatal(err)
	}
	// a proposal lands on the ask before the restart asks again
	pre := CaseAction{Action: ActExtend, Args: map[string]any{"sessions": 1}}.PgPreselected()
	if cs, _ := g.s.OpenCasesOf(ctx, "t1"); len(cs) == 1 {
		if err := g.s.SetQuestionProposal(ctx, "t1", cs[0].ID, "extend: one more session fixes it", "the factory agent", pre); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.r.FaceStop(ctx, "t1", HealedReason, nil); err != nil {
		t.Fatal(err)
	}
	asks := g.asks(t)
	if len(asks) != 2 || asks[1].Detail["reason"] != HealedReason {
		t.Fatalf("asks = %+v, want the ask and its restart's", asks)
	}
	d, err := g.s.CaseDesk(ctx, "demo-app/demo-app")
	if err != nil || len(d.Cases) != 1 {
		t.Fatalf("desk = %+v, %v", d, err)
	}
	c := d.Cases[0]
	card, _, _ := g.s.Task(ctx, "t1")
	face := CaseFace(c, card, d.Asks[c.ID])
	first := CaseFace(c, card, CaseAsk{Detail: asks[0].Detail})
	if face.Ask == "" || len(face.Offers) == 0 || face.Ask != first.Ask || face.Why != first.Why || face.Story != first.Story ||
		face.Info != first.Info || !reflect.DeepEqual(face.Offers, first.Offers) {
		t.Errorf("face after the restart = %+v, want the first ask's %+v", face, first)
	}
	if face.Recommendation != "extend: one more session fixes it" || face.Source != "the factory agent" || face.Preselected != ActExtend {
		t.Errorf("face after the restart = %+v, want the proposal carried over: recommendation, writer and preselected action", face)
	}
	if !strings.Contains(face.Why, "session") || strings.Contains(face.Why, "restarted") {
		t.Errorf("Why = %q, want the case's own reason", face.Why)
	}
}

// A human card's ask and a plan question's reach the face with their info,
// their choices and the choice their asker recommends.
func TestHumanCardAndPlanQuestionAsksReachTheFace(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	task := Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}
	if err := g.s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	q := HumanQuestion{Ask: "Which store keeps the snapshots?", Why: "Two fit.", Info: "Postgres is shared.", Recommendation: "Postgres", Recommended: "say:Postgres", Source: "the agent",
		Choices: []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}, {Value: "say:SQLite", Label: "SQLite"}}}
	if err := recordAsk(ctx, g.r.PgTimeline(), task, q, "factoryd", "chat", ViaFactoryd, "", nil); err != nil {
		t.Fatal(err)
	}
	d, err := g.s.CaseDesk(ctx, "demo-app/demo-app")
	if err != nil || len(d.Cases) != 1 {
		t.Fatalf("desk = %+v, %v", d, err)
	}
	c := d.Cases[0]
	face := CaseFace(c, task, d.Asks[c.ID])
	if face.Info != "Postgres is shared." || len(face.Offers) != 2 || face.Offers[0].Label != "Postgres" || face.Offers[1].Label != "SQLite" ||
		face.Preselected != "choice:say:Postgres" || face.Recommendation != "Postgres" || face.Source != "the agent" {
		t.Errorf("the human card's face = %+v", face)
	}
	rev, err := g.s.AddPlanRevision(ctx, "t1", json.RawMessage(`{"summary": "one"}`), "s1")
	if err != nil {
		t.Fatal(err)
	}
	planTask, _, _ := g.r.PgTask(ctx, "t1")
	changes := []string{"The service keeps its rows in the shared Postgres.", "The service keeps its rows in one SQLite file."}
	if err := g.r.AskQuestions(ctx, planTask, rev, []PlanQuestion{{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}, Changes: changes, Recommended: "Postgres", Grounds: "it is shared"}}); err != nil {
		t.Fatal(err)
	}
	if stored, err := g.s.PlanQuestions(ctx, "t1", rev); err != nil || len(stored) != 1 || !slices.Equal(stored[0].Changes, changes) {
		t.Errorf("the stored question = %+v, %v; want its changes %q", stored, err, changes)
	}
	d, _ = g.s.CaseDesk(ctx, "demo-app/demo-app")
	var plan Case
	for _, k := range d.Cases {
		if k.Kind == CasePlanQuestion {
			plan = k
		}
	}
	if plan.ID == 0 {
		t.Fatalf("no plan question case in %+v", d.Cases)
	}
	pf := CaseFace(plan, planTask, d.Asks[plan.ID])
	if len(pf.Offers) != 3 || pf.Offers[0].Value != "plan:Postgres" || pf.Offers[1].Value != "plan:SQLite" ||
		pf.Preselected != "plan:Postgres" || pf.Recommendation != "it is shared" || pf.Source != "the planner" ||
		pf.Offers[0].Effect != changes[0] || pf.Offers[1].Effect != changes[1] {
		t.Errorf("the plan question's face = %+v", pf)
	}
}

// The factory agent's statement for a human card's case, and for a plan
// question's, offers that ask's own choices and never the action table of
// a card.
func TestRungTwoStatementOffersTheAsksOwnChoices(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.PgTask(ctx, "h1")
	q := HumanQuestion{Ask: "Which store?", Why: "Two fit.", Info: "Postgres is shared.", Choices: []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}, {Value: "say:SQLite", Label: "SQLite"}}}
	if err := recordAsk(ctx, g.r.PgTimeline(), h, q, "factoryd", "agent", ViaChat, "", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "h1")
	if len(cases) != 1 || cases[0].Kind != CaseHumanCard {
		t.Fatalf("cases = %+v; want the chat-escalated card's one human card case", cases)
	}
	text, err := g.r.RungTwoStatement(ctx, cases[0].ID)
	if err != nil || !strings.HasPrefix(text, "Which store?\n") || !strings.Contains(text, "The ask's choices:\n- say:Postgres: Postgres\n- say:SQLite: SQLite\n") ||
		strings.Contains(text, "- resume") || strings.Contains(text, "- handover") {
		t.Errorf("the human card's statement = %q, %v; want its problem and the ask's own two choices, no action table", text, err)
	}

	g.r.PgTimeline().RecordRow(ctx, "t1", EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"question": "Which database?", "revision": 1, "index": 1,
		"choices": []string{"Postgres", "SQLite", "MySQL"}, "offers": []QuestionChoice{{Value: "Postgres", Label: "Postgres"}, {Value: "SQLite", Label: "SQLite"}, {Value: "MySQL", Label: "MySQL"}}})
	plan, _ := g.s.OpenCasesOf(ctx, "t1")
	var pq Case
	for _, c := range plan {
		if c.Kind == CasePlanQuestion {
			pq = c
		}
	}
	ptext, err := g.r.RungTwoStatement(ctx, pq.ID)
	if err != nil || !strings.HasPrefix(ptext, "Which database?\n") || !strings.Contains(ptext, "The ask's choices:\n- Postgres: Postgres\n- SQLite: SQLite\n- MySQL: MySQL\n") || strings.Contains(ptext, "- resume") {
		t.Errorf("the plan question's statement = %q, %v", ptext, err)
	}
}

// Rung 2's statement for a plan's confirmation offers the confirmation where
// the guard passes it: at full auto on a revision that takes nothing off
// main, and names the human's press on one that takes merged work off main.
func TestRungTwoStatementOffersTheConfirmationWhereTheGuardPasses(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	for i, takeOff := range [][]string{nil, {"api"}, nil} {
		raw, _ := json.Marshal(Plan{TakeOffMain: takeOff})
		rev, err := g.s.AddPlanRevision(ctx, "t1", raw, fmt.Sprintf("sha-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		g.r.PgTimeline().RecordRow(ctx, "t1", EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"question": "Does the plan match the spec?", "revision": rev, "index": 1, "kind": "confirmation"})
		var c Case
		cases, _ := g.s.OpenCasesOf(ctx, "t1")
		for _, k := range cases {
			if k.Kind == CasePlanConfirmation && k.Facts["revision"] == float64(rev) {
				c = k
			}
		}
		if i == 2 {
			if ok, err := g.s.PutPlanProofs(ctx, "t1", rev, []PlanProof{{Idx: 1, ID: "door_state", Origin: ProofOriginHoldout, Claim: "c", Inputs: "i", Expected: "e", State: ProofProposed}}); err != nil || !ok {
				t.Fatalf("hold-out: %v %v", ok, err)
			}
		}
		text, err := g.r.RungTwoStatement(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && (strings.Contains(text, "You may confirm") || !strings.Contains(text, "Hold-out candidates of this revision are undecided")) {
			t.Errorf("revision %d with a hold-out undecided: statement %q", rev, text)
		}
		if i == 0 && !strings.Contains(text, "You may confirm this plan with the answer tool (revision 1, question 1, decision confirm)") ||
			i == 1 && (strings.Contains(text, "You may confirm") || !strings.Contains(text, "takes merged work off main, or its reverts could not be read, so its confirmation is the human's, made with Confirm the plan on the plan's card.")) {
			t.Errorf("revision %d (takes off main %v): statement %q", rev, takeOff, text)
		}
	}
}

// A rung's recommendation is its grounds, empty without them, and names
// its writer: the face names the action by its answer's label, never by
// the action's name.
func TestRecommendationNeedsGroundsAndNamesItsWriter(t *testing.T) {
	g := newTripRig(t, "success")
	if got := g.r.PgRecommendation(Task{}, &HandUp{Action: ActClose, Grounds: "  "}); got != "" {
		t.Errorf("a recommendation with no grounds = %q, want none", got)
	}
	if got := g.r.PgRecommendation(Task{}, &HandUp{Action: ActClose, Grounds: "The work is on main."}); got != "The work is on main." {
		t.Errorf("recommendation = %q, want the grounds alone", got)
	}
	if recommendedBy(&HandUp{ActionRung: 2}) != "the factory agent" || recommendedBy(&HandUp{ActionRung: 1}) != "the check-in" {
		t.Error("the writers' names are not the factory agent's and the check-in's")
	}
}

// A statement lists the choices of its own case's ask: a card with two live
// human asks gives each case the choices it asked.
func TestRungTwoStatementTakesItsOwnCasesAsk(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.PgTask(ctx, "h1")
	for _, q := range []HumanQuestion{
		{Ask: "Which store?", Why: "w", Info: "i", Choices: []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}}},
		{Ask: "Which port?", Why: "w", Info: "i", Choices: []QuestionChoice{{Value: "say:8080", Label: "8080"}}},
	} {
		if err := recordAsk(ctx, g.r.PgTimeline(), h, q, "factoryd", "webhook", ViaFactoryd, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	cases, _ := g.s.OpenCasesOf(ctx, "h1")
	if len(cases) != 2 {
		t.Fatalf("cases = %+v; want two", cases)
	}
	for i, want := range []string{"- say:Postgres: Postgres\n", "- say:8080: 8080\n"} {
		text, err := g.r.RungTwoStatement(ctx, cases[i].ID)
		other := []string{"say:8080", "say:Postgres"}[i]
		if err != nil || !strings.Contains(text, want) || strings.Contains(text, other) {
			t.Errorf("case %d's statement = %q, %v; want its own choice %q and not %q", cases[i].ID, text, err, want, other)
		}
	}
}

// A trip on a chat-escalated human card opens a human card case, as the
// ask's own event does: one helper decides it for both.
func TestTripCaseMakesAChatEscalatedHumanCardAHumanCard(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.PgTask(ctx, "h1")
	c, _, err := g.r.PgTripCase(ctx, h, CaseFacts{}, TripInput{TaskID: "h1", Reason: AgentAsks + "Which store?"})
	if err != nil || c.Kind != CaseHumanCard {
		t.Fatalf("trip case = %+v, %v; want a human card case", c, err)
	}
	agent := Task{ID: "a1", Kind: "agent"}
	if HumanCardKind(CaseAgentEscalate, agent) != CaseAgentEscalate || HumanCardKind(CaseMonthCap, h) != CaseMonthCap || HumanCardKind(CaseOther, h) != CaseHumanCard {
		t.Error("humanCardKind turns an agent card's ask, or a specialised kind, into a human card")
	}
}

// The agent's request for information puts its case at the human: the card
// that waits for its issue's author asks the request on the board, with
// "Answer in my words", "Not now" and "End the card". The answer's press
// posts the human's words as the issue comment the run reads, and ends the
// case; an answer the author writes on the issue ends it too.
func TestAnInformationRequestAsksTheHuman(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	question := "Which Hetzner location does the load test's server use, fsn1 or nbg1?"
	reason := AgentAsk{Kind: AskInfo, Question: question, Tried: "both regions' price lists", Needs: "the location the server runs in"}.PgReason()
	asked := func() (Case, CaseAsk) {
		t.Helper()
		cs, err := g.s.CasesOf(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(cs, func(c Case) bool { return c.Status == CaseAtHuman })
		if i < 0 {
			t.Fatalf("cases %+v, want one at the human", cs)
		}
		ask, ok := g.r.CaseAsk(ctx, cs[i])
		if !ok {
			t.Fatalf("case %d has no ask row", cs[i].ID)
		}
		return cs[i], ask
	}
	comment, err := g.r.RequestInfo(ctx, "t1", reason)
	if err != nil {
		t.Fatal(err)
	}
	c, ask := asked()
	face := CaseFace(c, Task{ID: "t1", Kind: "agent"}, ask)
	var offers []string
	for _, o := range face.Offers {
		offers = append(offers, o.Label)
	}
	if c.Kind != CaseAgentRequestInfo || face.Ask != question || !slices.Equal(offers, []string{"Answer in my words", "Not now", "End the card"}) || c.CommentID != comment {
		t.Fatalf("case %s by comment %d asks %q with %v; want the request asking %q with its three answers, by comment %d", c.Kind, c.CommentID, face.Ask, offers, question, comment)
	}
	var posted []string
	out, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActContinue, Note: "fsn1"}, Post: func(context.Context) (int64, error) { posted = append(posted, "fsn1"); return comment + 1, nil }})
	if err != nil {
		t.Fatalf("the answer's press = %v", err)
	}
	if len(posted) != 1 || out.Comment != comment+1 || caseByID(t, g.s, c.ID).Status != CaseResolved {
		t.Fatalf("posted %v, comment %d, case %s; want the human's words posted once as comment %d and the case resolved", posted, out.Comment, caseByID(t, g.s, c.ID).Status, comment+1)
	}
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'running' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	comment, err = g.r.RequestInfo(ctx, "t1", reason)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = asked()
	if _, err := g.s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "t1", Kind: EventQuestionAnswered, Actor: "alice", Via: ViaWebhook,
		Detail: map[string]any{"comment": comment + 5}}); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID).Status; got != CaseResolved {
		t.Fatalf("after the author's comment the request's case is %s, want it resolved", got)
	}
}

// A card that waits on its issue's author without the request's case, one
// that asked before the board did or whose ask failed to write, gets the
// case from the request's anchor at the minute pass, once, and none while
// the request itself still runs.
func TestAWaitOnTheAuthorWithoutItsCaseGetsItAtTheMinutePass(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	question := "Which Hetzner location does the load test's server use, fsn1 or nbg1?"
	reason := AgentAsk{Kind: AskInfo, Question: question, Tried: "both regions' price lists", Needs: "the location the server runs in"}.PgReason()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'waiting-poster' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: IntakeInfoRequest, Author: "factoryd", Body: reason, CommentID: 41}); err != nil {
		t.Fatal(err)
	}
	asked := func(when string) {
		t.Helper()
		cs, err := g.s.CasesOf(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != 1 || cs[0].Kind != CaseAgentRequestInfo || cs[0].Status != CaseAtHuman || cs[0].CommentID != 41 {
			t.Fatalf("%s: cases %+v, want the request's one case at the human by comment 41", when, cs)
		}
		steps, err := g.s.CaseSteps(ctx, cs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		ask, _ := g.r.CaseAsk(ctx, cs[0])
		if face := CaseFace(cs[0], Task{ID: "t1", Kind: "agent"}, ask); len(steps) != 1 || face.Ask != question {
			t.Fatalf("%s: %d steps asking %q, want the one ask of %q", when, len(steps), face.Ask, question)
		}
	}
	lock := g.r.PgRequestLock("t1")
	lock.Lock()
	g.r.StoreMissedOffers(ctx)
	if cs, _ := g.s.CasesOf(ctx, "t1"); len(cs) != 0 {
		t.Fatalf("while the request runs, the pass opened %+v", cs)
	}
	lock.Unlock()
	g.r.StoreMissedOffers(ctx)
	asked("after one pass")
	g.r.StoreMissedOffers(ctx)
	asked("after another pass")
}

// A request knows itself across its activity's retries by the mark its
// anchor stores, not by its words: an earlier request of the same words is
// another request. A retry after the comment failed posts the request and
// asks it; a retry after it finished posts nothing and asks nothing again.
func TestARequestsRetryKnowsItsOwnRequest(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	question := "Which Hetzner location does the load test's server use, fsn1 or nbg1?"
	reason := AgentAsk{Kind: AskInfo, Question: question, Tried: "both regions' price lists", Needs: "the location the server runs in"}.PgReason()
	if _, err := g.s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: IntakeInfoRequest, Author: "factoryd", Body: reason, CommentID: 41}); err != nil {
		t.Fatal(err)
	}
	g.forge.failComment = true
	if _, err := execActivity(t, g.r, ActivityRequestInfo, "t1", reason); err == nil {
		t.Fatal("the request posted while its comment failed")
	}
	for _, attempt := range []string{"the retry", "a retry after it finished"} {
		v, err := execActivity(t, g.r, ActivityRequestInfo, "t1", reason)
		var id int64
		if err == nil {
			err = v.Get(&id)
		}
		if err != nil || id != 501 || len(g.forge.comments) != 1 {
			t.Fatalf("%s = %d, %v, with %d comments posted; want comment 501, posted once", attempt, id, err, len(g.forge.comments))
		}
		cs, err := g.s.CasesOf(ctx, "t1")
		if err != nil || len(cs) != 1 || cs[0].CommentID != 501 || cs[0].Status != CaseAtHuman {
			t.Fatalf("%s: cases %+v, %v; want the request's one case at the human by comment 501", attempt, cs, err)
		}
		if steps, _ := g.s.CaseSteps(ctx, cs[0].ID); len(steps) != 1 {
			t.Fatalf("%s: %d steps, want the one ask", attempt, len(steps))
		}
		if task, _, _ := g.s.Task(ctx, "t1"); task.Status != "waiting-poster" {
			t.Fatalf("%s: the card is %s, want waiting-poster", attempt, task.Status)
		}
	}
}

// The project agent answers the request in the chat: its press posts the
// words on the issue once, as factory-chat, whose comment the wait hears as
// a writer's; the press ends the case and tells the run the answer is whole.
func TestTheChatAnswersAnInformationRequest(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	reason := AgentAsk{Kind: AskInfo, Question: "Which Hetzner location does the load test's server use, fsn1 or nbg1?",
		Tried: "both regions' price lists", Needs: "the location the server runs in"}.PgReason()
	comment, err := g.r.RequestInfo(ctx, "t1", reason)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := g.s.OpenCasesOf(ctx, "t1")
	if err != nil || len(cs) != 1 {
		t.Fatalf("cases %+v, %v; want the request's one case", cs, err)
	}
	posted := 0
	out, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: cs[0].ID, By: ChatUser, Via: ViaChat, Rung: 2,
		Res: Resolution{Action: ActContinue, Note: "fsn1"}, Post: func(context.Context) (int64, error) { posted++; return comment + 1, nil }})
	if err != nil || posted != 1 || out.Comment != comment+1 || caseByID(t, g.s, cs[0].ID).Status != CaseResolved {
		t.Fatalf("the chat's press = %v, posted %d times as comment %d, case %s; want one post as comment %d and the case resolved",
			err, posted, out.Comment, caseByID(t, g.s, cs[0].ID).Status, comment+1)
	}
	whole := slices.ContainsFunc(g.signals.got, func(c reopenCall) bool {
		return c.signal == SignalAnswerWhole && c.sig.Comment == comment+1 && c.sig.Author == ChatUser
	})
	if !whole {
		t.Fatalf("signals %+v, want the answer whole by %s at comment %d", g.signals.got, ChatUser, comment+1)
	}
}

// A card that ended or moved on while its session ended asks its author
// nothing: the request refuses before it posts.
func TestARequestOfACardThatCannotWaitPostsNothing(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'closing' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	reason := AgentAsk{Kind: AskInfo, Question: "Which Hetzner location does the load test's server use, fsn1 or nbg1?"}.PgReason()
	if _, err := g.r.RequestInfo(ctx, "t1", reason); !isTerminal(err) || len(g.forge.comments) != 0 {
		t.Fatalf("the request of a closing card = %v with %d comments posted; want a terminal refusal and nothing posted", err, len(g.forge.comments))
	}
	if events, _ := g.s.IntakeEvents(ctx, "t1"); len(events) != 0 {
		t.Fatalf("intake events %+v, want no anchor", events)
	}
}

// downTimeline is an object store whose read of t1's timeline fails while
// down is set.
type downTimeline struct {
	ObjectStore
	down bool
}

func (o *downTimeline) Get(ctx context.Context, key string) ([]byte, error) {
	if o.down && key == "sessions/t1/timeline.jsonl" {
		return nil, errors.New("the object store is down")
	}
	return o.ObjectStore.Get(ctx, key)
}

// The human's buttons on a case that reached them while the card's
// timeline could not be read are no final word: the face stores the
// offers it could weigh and the mark that it is unweighed, so the offers
// pass weighs them again, once the timeline reads, and Reverify comes
// back with the mark gone; a pass while it still fails stores nothing.
func TestAFaceOnAnUnreadTimelineIsWeighedAgain(t *testing.T) {
	ctx := context.Background()
	g := newTripRig(t, "success")
	g.atHead(t, true)
	objects := &downTimeline{ObjectStore: g.r.PgObjects(), down: true}
	g.r.PgSetObjects(objects)
	// a gate stop's face offers Reverify, which the table refuses on an unread timeline
	if _, err := g.r.FaceStop(ctx, "t1", GateStopped+ActivityModelReview+" after 3 tries: the review timed out", nil); err != nil {
		t.Fatal(err)
	}
	offered := func() ([]string, bool) {
		asks := g.asks(t)
		detail := asks[len(asks)-1].Detail
		var names []string
		offers, _ := detail["offers"].([]any)
		for _, o := range offers {
			if m, ok := o.(map[string]any); ok {
				names = append(names, fmt.Sprint(m["value"]))
			}
		}
		_, unweighed := detail["unweighed"]
		return names, !unweighed
	}
	if names, weighed := offered(); weighed || slices.Contains(names, ActReverify) {
		t.Fatalf("the face on an unread timeline offers %v, weighed %t; want no reverify and the face unweighed", names, weighed)
	}
	g.r.StoreMissedOffers(ctx)
	if names, weighed := offered(); weighed || slices.Contains(names, ActReverify) {
		t.Fatalf("a pass on a still unread timeline stored %v, weighed %t; want nothing stored", names, weighed)
	}
	objects.down = false
	g.r.StoreMissedOffers(ctx)
	if names, weighed := offered(); !weighed || !slices.Contains(names, ActReverify) {
		t.Fatalf("the pass on a read timeline stored %v, weighed %t; want reverify on a weighed face", names, weighed)
	}
}
