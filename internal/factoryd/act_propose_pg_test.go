package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const leaky = "see https://user:pass@host/x with Authorization: Bearer abc123def456"

func TestProposesByLevel(t *testing.T) {
	for _, tc := range []struct {
		state, action string
		want          bool
	}{
		{policy.AutonomyFullAuto, ActExtend, false},
		{policy.AutonomyFullAuto, ActRetry, false},
		{policy.AutonomyAutonomous, ActExtend, false},
		{policy.AutonomyAutonomous, ActRetry, false},
		{policy.AutonomyAutonomous, ActClose, false},
		{policy.AutonomyAutonomous, ActApproveMerge, true},
		{policy.AutonomyAssisted, ActRetry, true},
		{policy.AutonomyAssisted, ActRouteApproval, false},
		{policy.AutonomyManual, ActRouteApproval, false},
		{policy.AutonomyAssisted, ActClose, true},
		{policy.AutonomyManual, ActExtend, true},
		{policy.AutonomyManual, ActReplan, true},
	} {
		if got := Proposes(tc.state, tc.action); got != tc.want {
			t.Errorf("Proposes(%s, %s) = %v, want %v", tc.state, tc.action, got, tc.want)
		}
	}
	for _, a := range Actions {
		if got := Proposes(policy.AutonomyAutonomous, a.Name); got != (a.Name == ActApproveMerge) {
			t.Errorf("action %s proposes at autonomous = %v: only the merge approval does", a.Name, got)
		}
	}
}

// atAutonomy puts the rig's project at state: full auto is autonomous with
// merge rule auto.
func (g rungRig) atAutonomy(state string) {
	v := g.r.cfg.Policy()
	p := v.Projects["demo-app"]
	p.Autonomy, p.MergeRule = state, policy.MergeHuman
	if state == policy.AutonomyFullAuto {
		p.Autonomy, p.MergeRule = policy.AutonomyAutonomous, policy.MergeAuto
	}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.cfg.Policy = func() policy.Version { return v }
}

func poolRows(t *testing.T, g rungRig) int {
	t.Helper()
	var n int
	if err := g.s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM events WHERE task_id = 't1' AND `+poolDraws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// At assisted and manual rung 2's extend is a proposal. By hand: no session
// is raised (raised_sessions 0), no pool row is booked, rung 2's one step
// ends handed up with the extend as its decision and its Did holding the
// action, its args and the result proposed, the human is asked once with
// the extend preselected, and the text says so. The secrets in the note and
// the finding are masked on the face.
func TestRung2ProposesAtAssistedAndManual(t *testing.T) {
	for _, state := range []string{policy.AutonomyAssisted, policy.AutonomyManual} {
		t.Run(state, func(t *testing.T) {
			g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
			g.atAutonomy(state)
			g.chat.answers = []CheckinResponse{g.handUp(t)}
			out := g.trip(t, tripCap)
			g.settle()
			res, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2,
				Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Fund: &policy.Draw{AgentTime: 10 * time.Minute}, Note: leaky}, Found: leaky})
			if err != nil {
				t.Fatalf("Act: %v", err)
			}
			if want := "Proposed extend on #12: the human confirms it on the card."; res.Text != want {
				t.Errorf("text %q, want %q", res.Text, want)
			}
			if g.raisedSessions(t) != 0 || poolRows(t, g) != 0 {
				t.Errorf("raised sessions %d, pool rows %d; want none", g.raisedSessions(t), poolRows(t, g))
			}
			own := g.stepsOfRung(t, out.CaseID, 2)
			if len(own) != 1 || own[0].Outcome != StepHandedUp || own[0].Decision != ActExtend || len(own[0].Did) != 1 ||
				own[0].Did[0].Action != ActExtend || own[0].Did[0].Result != "proposed" || own[0].Did[0].Args["sessions"] != float64(1) {
				t.Fatalf("rung 2 steps %+v, want one handed up with the extend proposed", own)
			}
			if own[0].Did[0].Fund == nil || own[0].Did[0].Fund.AgentTime != 10*time.Minute || strings.Contains(own[0].Grounds, "Your one job") {
				t.Errorf("step %+v, want the fund of 10 minutes and the rung's own note without the session's lead", own[0])
			}
			asks := g.asks(t)
			if len(asks) != 1 {
				t.Fatalf("%d asks, want one", len(asks))
			}
			pre, _ := asks[0].Detail["preselected"].(map[string]any)
			if fund, _ := pre["fund"].(map[string]any); pre["action"] != ActExtend || pre["args"].(map[string]any)["sessions"] != float64(1) || fund["agent_time"] == nil {
				t.Errorf("preselected %v, want extend with one session", asks[0].Detail["preselected"])
			}
			if asks[0].Detail["recommended_by"] != "the factory agent" {
				t.Errorf("the proposal's writer = %v, want the factory agent", asks[0].Detail["recommended_by"])
			}
			face := strings.Join(g.forge.comments, "\n") + asks[0].Detail["recommendation"].(string) + asks[0].Detail["info"].(string)
			for _, secret := range []string{"user:pass", "abc123def456"} {
				if strings.Contains(face, secret) || strings.Contains(own[0].Grounds+own[0].Found, secret) {
					t.Errorf("the face or the step holds %q", secret)
				}
			}
			if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman {
				t.Errorf("case %s, want at the human", c.Status)
			}
		})
	}
}

// At autonomous and at full auto the same call runs: one session is raised
// and the case ends resolved on rung 2.
func TestRung2RunsAtAutonomousAndFullAuto(t *testing.T) {
	for _, state := range []string{policy.AutonomyAutonomous, policy.AutonomyFullAuto} {
		t.Run(state, func(t *testing.T) {
			g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
			g.atAutonomy(state)
			g.chat.answers = []CheckinResponse{g.handUp(t)}
			out := g.trip(t, tripCap)
			g.settle()
			if _, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2,
				Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "one more"}}); err != nil {
				t.Fatalf("Act: %v", err)
			}
			if c := caseByID(t, g.s, out.CaseID); c.Status != CaseResolved || g.raisedSessions(t) != 1 || len(g.asks(t)) != 0 {
				t.Errorf("case %s, raised %d, asks %d; want resolved, 1 and none", c.Status, g.raisedSessions(t), len(g.asks(t)))
			}
		})
	}
}

const policyClassReason = "check-in: " + CheckPolicyClass + ": the change touches a migration"

// A policy class on a green, approved head goes to its merge approval at
// rung 0, resolved with route_approval and no ask.
func TestPolicyClassOnAnApprovedHeadRoutesToApproval(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.atHead(t, true)
	out := g.trip(t, policyClassReason)
	c := caseByID(t, g.s, out.CaseID)
	if c.Kind != CasePolicyClass || out.Resolution == nil || out.Resolution.Action != ActRouteApproval || c.Status != CaseResolved || len(g.asks(t)) != 0 {
		t.Fatalf("case %s %s, outcome %+v, asks %d; want a policy class resolved by route_approval", c.Kind, c.Status, out.Resolution, len(g.asks(t)))
	}
}

// A policy class whose head's checks are not green cannot route to its
// approval (a review's concerns no longer stop it): it waits for rung 2's
// analysis alone, rung 2 may take no action on it, and its analysis faces
// the human once with the recommendation, secrets masked.
func TestPolicyClassFacesWithRung2sAnalysisOnly(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.forge.checks["h2"] = "pending"
	out := g.trip(t, policyClassReason)
	g.settle()
	if out.Rung != 2 || g.chat.calls != 0 {
		t.Fatalf("outcome %+v, rung 1 calls %d; want rung 2 on the case, rung 1 asked nothing", out, g.chat.calls)
	}
	if one := g.stepsOfRung(t, out.CaseID, 1); len(one) != 1 || one[0].Outcome != StepHandedUp || one[0].Decision != CheckHuman {
		t.Fatalf("rung 1 steps %+v, want the check-in's own human answer as one handed-up step", one)
	}
	ctx := context.Background()
	_, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActClose}})
	want := "this case is the human's by its kind (the merge gate marks the policy class human): record your analysis and recommended option with attach_analysis"
	if err == nil || err.Error() != want {
		t.Fatalf("Act close = %v, want %q", err, want)
	}
	handled, err := g.r.HandUpRung2(ctx, "t1", out.CaseID, "the migration drops a column: "+leaky, RungTwoRecommendation{Action: ActClose, Grounds: leaky}, nil)
	if err != nil || handled != out.CaseID {
		t.Fatalf("HandUpRung2 = %v, %v", handled, err)
	}
	asks := g.asks(t)
	if len(asks) != 1 || !strings.HasPrefix(asks[0].Detail["recommendation"].(string), "close: ") {
		t.Fatalf("asks %+v, want one with the recommendation", asks)
	}
	if pre, _ := asks[0].Detail["preselected"].(map[string]any); pre["action"] != ActClose {
		t.Errorf("preselected %v, want the analysis's recommended close", asks[0].Detail["preselected"])
	}
	if own := g.stepsOfRung(t, out.CaseID, 2); len(own) != 1 || len(own[0].Did) != 1 || own[0].Did[0].Result != "recommended" {
		t.Errorf("rung 2 steps %+v, want the close recorded as recommended", own)
	}
	face := strings.Join(g.forge.comments, "\n") + asks[0].Detail["recommendation"].(string) + asks[0].Detail["info"].(string)
	for _, secret := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(face, secret) {
			t.Errorf("the face holds %q", secret)
		}
	}
}

// A closed dependency goes to rung 2 with the closer's reason and no rung 1
// conversation. By hand: the case climbs from rung 0 to rung 2 (one
// case_climbed row), the model of rung 1 is never called, and the
// statement rung 2 reads holds the reason of the dependency's end.
func TestDependencyClosedGoesToRung2WithTheClosersReason(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "dep1", Kind: "agent", Status: "closed", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 1451, Track: policy.TrackDirect}); err != nil {
		t.Fatal(err)
	}
	if err := g.s.AddTaskDeps(ctx, "t1", []string{"dep1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Record(ctx, Event{Kind: EventTaskEnded, TaskID: "dep1", Repository: "demo-app/demo-app", IssueNumber: 1451, Actor: "sebastian", Via: ViaConsole,
		Detail: map[string]any{"reason": "issue closed by sebastian"}}); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, depEndedReason(Dependency{TaskID: "dep1", Issue: 1451, Key: "api", Status: "closed"}))
	g.settle()
	c := caseByID(t, g.s, out.CaseID)
	if out.Rung != 2 || c.Kind != CaseDependencyClosed || len(g.rows(t, EventCaseClimbed)) != 1 || g.chat.calls != 0 || len(g.stepsOfRung(t, out.CaseID, 1)) != 0 {
		t.Fatalf("outcome %+v, kind %s, rung 1 calls %d; want the case at rung 2 with no rung 1", out, c.Kind, g.chat.calls)
	}
	if c.Facts["closed_why"] != "issue closed by sebastian" {
		t.Errorf("closed_why = %v", c.Facts["closed_why"])
	}
	text, err := g.r.RungTwoStatement(ctx, out.CaseID)
	if err != nil || !strings.Contains(text, "issue closed by sebastian") {
		t.Fatalf("statement %q, %v; want the closer's reason", text, err)
	}
}

// The closer's reason is masked in the statement rung 2 reads.
func TestClosedWhyIsMaskedInTheStatement(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "dep1", Kind: "agent", Status: "closed", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 1451, Track: policy.TrackDirect}); err != nil {
		t.Fatal(err)
	}
	if err := g.s.AddTaskDeps(ctx, "t1", []string{"dep1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Record(ctx, Event{Kind: EventTaskEnded, TaskID: "dep1", Repository: "demo-app/demo-app", IssueNumber: 1451, Actor: "sebastian", Via: ViaConsole,
		Detail: map[string]any{"reason": "issue closed, " + leaky}}); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, depEndedReason(Dependency{TaskID: "dep1", Issue: 1451, Key: "api", Status: "closed"}))
	g.settle()
	text, err := g.r.RungTwoStatement(ctx, out.CaseID)
	if err != nil || !strings.Contains(text, "issue closed") || strings.Contains(text, "user:pass") || strings.Contains(text, "abc123def456") {
		t.Fatalf("statement %q, %v; want the reason with both secrets masked", text, err)
	}
	if c := caseByID(t, g.s, out.CaseID); strings.Contains(c.Facts["closed_why"].(string), "user:pass") {
		t.Errorf("closed_why %v holds a secret", c.Facts["closed_why"])
	}
}

// A queued_over_bound monitor flag is a board-level case: it goes to rung 2
// with no rung 1 conversation, as a session flag would not.
func TestQueuedOverBoundGoesToRung2WithoutRung1(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	out, err := g.r.OpenTrip(context.Background(), TripInput{TaskID: "t1", Reason: monitorFlagPrefix + "queued_over_bound: the card waited 3h", Head: "h2", Kind: CaseMonitorFlag,
		StateKey: "queued|x", Extra: map[string]any{"flag": "queued_over_bound"}}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.settle()
	if out.Rung != 2 || g.chat.calls != 0 || len(g.stepsOfRung(t, out.CaseID, 1)) != 0 || len(g.stepsOfRung(t, out.CaseID, 2)) != 1 {
		t.Fatalf("outcome %+v, rung 1 calls %d; want rung 2 on the flag with rung 1 untouched", out, g.chat.calls)
	}
}

// A board-level case and a case only a human decides, which skip rung 1,
// meet the spent judgment budget in startRung2: each faces the human with
// the mark on its problem, rung 0's step naming the chat task's money and
// no rung 2 step.
func TestSkippedRung1KindsMeetTheSpentJudgmentBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		reason string
		judged bool
	}{
		"a closed dependency": {depEndedReason(Dependency{TaskID: "dep1", Issue: 1451, Key: "api", Status: "closed"}), false},
		"a policy class":      {policyClassReason, true},
	} {
		reason := tc.reason
		t.Run(name, func(t *testing.T) {
			g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
			g.forge.checks["h2"] = "pending" // a policy class on a green head routes to its approval at rung 0
			g.r.store = projectStore{g.s}
			cardForge, _ := newAlertForgejo(t)
			g.r.ChatBudget = &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: cardForge, Timeline: g.r.timeline}
			ctx := context.Background()
			if err := g.s.CreateTask(ctx, Task{ID: "dep1", Kind: "agent", Status: "closed", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 1451, Track: policy.TrackDirect}); err != nil {
				t.Fatal(err)
			}
			if err := g.s.AddTaskDeps(ctx, "t1", []string{"dep1"}); err != nil {
				t.Fatal(err)
			}
			chat := g.spendChat(t)
			if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur WHERE id = $1`, chat.ID); err != nil {
				t.Fatal(err)
			}
			out := g.trip(t, reason)
			c := caseByID(t, g.s, out.CaseID)
			want := fmt.Sprintf("not judged, judgment budget spent: the chat task %s has 0.00 of 10.00 EUR left", chat.ID)
			if tc.judged {
				want = fmt.Sprintf("%s The chat task %s has 0.00 of 10.00 EUR left.", noJudgmentLine, chat.ID)
			}
			// a case no rung judged is marked; a policy class was judged by the check-in's
			// moment, so its words stay and the face carries the one line
			if out.Rung != 3 || len(g.stepsOfRung(t, c.ID, 2)) != 0 || len(g.rows(t, EventCaseClimbed)) != 0 || strings.HasSuffix(c.Problem, unjudgedSuffix) == tc.judged ||
				!slices.ContainsFunc(g.steps(t, c.ID), func(s CaseStep) bool { return s.Rung == 0 && s.Found == want }) {
				t.Fatalf("outcome %+v, problem %q, steps %+v; want the spent path in full", out, c.Problem, g.steps(t, c.ID))
			}
			info := ""
			for _, a := range g.asks(t) {
				info += fmt.Sprint(a.Detail["info"])
			}
			if strings.Contains(info, noJudgmentLine) != tc.judged {
				t.Errorf("info %q, want the no-judgment line only for a judged case", info)
			}
		})
	}
}

// A policy class on a green head whose review raised concerns goes to its
// approval at rung 0 too: route_approval re-runs the gates, which send the
// concerns back to the agent as they do for any head, and no ask opens.
func TestPolicyClassOnAHeadWithConcernsRoutesToApproval(t *testing.T) {
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.atHead(t, false)
	out := g.trip(t, policyClassReason)
	c := caseByID(t, g.s, out.CaseID)
	if c.Kind != CasePolicyClass || out.Resolution == nil || out.Resolution.Action != ActRouteApproval || c.Status != CaseResolved || len(g.asks(t)) != 0 {
		t.Fatalf("case %s %s, outcome %+v, asks %d; want a policy class resolved by route_approval", c.Kind, c.Status, out.Resolution, len(g.asks(t)))
	}
}
