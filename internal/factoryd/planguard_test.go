package factoryd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	humanConfirmButton = "made with Confirm the plan on the plan's card."
	belowFullAutoWord  = "below full auto is the human's"
	offMainWord        = "takes merged work off main"
)

// By hand: a human passes at every level and on every revision. The agent
// passes a question only at full auto; its confirmation only at full auto
// and only when the revision takes nothing off main; its Request changes
// is an answer like a question's.
func TestPlanAnswerRefusal(t *testing.T) {
	for _, c := range []struct {
		name                         string
		rung                         int
		autonomy                     string
		confirming, off              bool
		refuses, wantWord, wantPress string
	}{
		{"a human at manual", 3, policy.AutonomyManual, true, true, "", "", ""},
		{"a human at full auto, taking work off main", 3, policy.AutonomyFullAuto, true, true, "", "", ""},
		{"the agent's confirmation at full auto", 2, policy.AutonomyFullAuto, true, false, "", "", ""},
		{"the agent's question at full auto", 2, policy.AutonomyFullAuto, false, false, "", "", ""},
		{"the agent's Request changes at full auto, taking work off main", 2, policy.AutonomyFullAuto, false, true, "", "", ""},
		{"the agent's confirmation at full auto, taking work off main", 2, policy.AutonomyFullAuto, true, true, "x", offMainWord, humanConfirmButton},
		{"the agent's confirmation at autonomous", 2, policy.AutonomyAutonomous, true, false, "x", belowFullAutoWord, humanConfirmButton},
		{"the agent's confirmation at assisted", 2, policy.AutonomyAssisted, true, false, "x", belowFullAutoWord, humanConfirmButton},
		{"the agent's question at autonomous", 2, policy.AutonomyAutonomous, false, false, "x", belowFullAutoWord, "on the plan's card"},
		{"the agent's answer with no level read", 2, "", false, false, "x", "autonomy level could not be read", "on the plan's card"},
		{"rung 1's confirmation at full auto", 1, policy.AutonomyFullAuto, true, false, "x", "The check-in decides no plan answer", "on the plan's card"},
		{"rung 1's question at full auto", 1, policy.AutonomyFullAuto, false, false, "x", "The check-in decides no plan answer", "on the plan's card"},
		{"rung 0's question at full auto", 0, policy.AutonomyFullAuto, false, false, "x", "The factory decides no plan answer", "on the plan's card"},
	} {
		got := PlanAnswerRefusal(c.rung, c.autonomy, c.confirming, c.off)
		if got != "" && strings.Contains(got, "attach_analysis") != (c.rung == 2) {
			t.Errorf("%s: refusal %q, want attach_analysis named for rung 2 alone", c.name, got)
		}
		if (got != "") != (c.refuses != "") || !strings.Contains(got, c.wantWord) || !strings.Contains(got, c.wantPress) {
			t.Errorf("%s: refusal %q, want one with %q and %q: %v", c.name, got, c.wantWord, c.wantPress, c.refuses != "")
		}
	}
}

// By hand: the human keeps or drops an extra promise at every level, the
// agent only at full auto, and a refusal names the project's level, the
// human's press and the agent's next step; the check-in decides none.
func TestHoldoutRefusal(t *testing.T) {
	const press = "kept or dropped with Keep it or Drop it on its page of the plan's card."
	for _, c := range []struct {
		name     string
		rung     int
		autonomy string
		want     string
	}{
		{"a human at manual", 3, policy.AutonomyManual, ""},
		{"the agent at full auto", 2, policy.AutonomyFullAuto, ""},
		{"the agent at autonomous", 2, policy.AutonomyAutonomous, "The project runs at autonomous, and below full auto an extra promise is the human's, " + press + planAgentStep},
		{"the agent at assisted", 2, policy.AutonomyAssisted, "The project runs at assisted, and below full auto an extra promise is the human's, " + press + planAgentStep},
		{"the agent with no level read", 2, "", "The project's autonomy level could not be read, so an extra promise stays the human's, " + press + planAgentStep},
		{"the check-in at full auto", 1, policy.AutonomyFullAuto, "The check-in decides no extra promise: it is the factory agent's at full auto, or the human's, " + press},
	} {
		if got := HoldoutRefusal(c.rung, c.autonomy); got != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
	}
}

// Rung 2's statement offers the confirmation only where the guard passes
// it: at full auto on a revision that takes nothing off main; on one that
// takes work off main it names the human's press and offers Request
// changes; below full auto it offers nothing. At full auto it lists each
// extra promise that waits with its claim, its inputs and what it expects,
// and offers to keep or drop it; below full auto it names none.
func TestPlanOfferFollowsTheGuard(t *testing.T) {
	confirm := Case{Kind: CasePlanConfirmation, Facts: map[string]any{"revision": float64(3), "index": float64(2)}}
	question := Case{Kind: CasePlanQuestion, Facts: map[string]any{"revision": float64(3), "index": float64(1)}}
	open := []PlanProof{{Idx: 4, Round: 3, ID: "gives_up", Origin: ProofOriginHoldout, State: ProofProposed,
		Claim: "The gateway gives up after 3 minutes without a store.", Inputs: "a store URL that refuses every connection", Expected: "exit code 1 within 4 minutes"}}
	const promise = "- holdout 4: The gateway gives up after 3 minutes without a store.\n  inputs: a store URL that refuses every connection\n  expected: exit code 1 within 4 minutes\n"
	const settle = "Keep or drop each with the answer tool (revision 3, holdout its number, decision keep or drop, your reason as text)"
	for _, c := range []struct {
		name     string
		c        Case
		autonomy string
		off      bool
		holdouts []PlanProof
		has, not []string
	}{
		{"confirmation at full auto", confirm, policy.AutonomyFullAuto, false, nil, []string{"You may confirm this plan with the answer tool (revision 3, question 2, decision confirm)"}, []string{"holdout"}},
		{"confirmation with hold-outs undecided", confirm, policy.AutonomyFullAuto, false, open,
			[]string{promise, settle, "Once each is kept or dropped, you may confirm this plan with the answer tool (revision 3, question 2, decision confirm), or request changes (decision revise, with your words)."},
			[]string{"You may confirm"}},
		{"hold-outs undecided on a revision taking work off main", confirm, policy.AutonomyFullAuto, true, open, []string{promise, settle, offMainWord, humanConfirmButton, "decision revise"}, []string{"may confirm"}},
		{"confirmation taking work off main", confirm, policy.AutonomyFullAuto, true, nil, []string{offMainWord, humanConfirmButton, "decision revise"}, []string{"You may confirm"}},
		{"confirmation at autonomous", confirm, policy.AutonomyAutonomous, false, nil, []string{belowFullAutoWord}, []string{"You may"}},
		{"hold-outs undecided at autonomous", confirm, policy.AutonomyAutonomous, false, open, []string{belowFullAutoWord}, []string{"You may", "holdout", "gives up"}},
		{"question at full auto", question, policy.AutonomyFullAuto, false, nil, []string{"You may answer this question with the answer tool (revision 3, question 1)"}, nil},
		{"question at assisted", question, policy.AutonomyAssisted, false, nil, []string{belowFullAutoWord}, []string{"You may"}},
	} {
		got := planOffer(c.c, c.autonomy, c.off, c.holdouts)
		for _, w := range c.has {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q holds %q", c.name, got, w)
			}
		}
	}
}

func planAskRig(t *testing.T, autonomy string, takeOff []string) (factsRig, Case) {
	t.Helper()
	g := newFactsRig(t)
	g.card()
	files := testFiles(testPolicyYAML)
	demo := demoFile
	if autonomy != policy.AutonomyFullAuto {
		demo = strings.Replace(strings.Replace(demoFile, "autonomy: autonomous\n", "autonomy: "+autonomy+"\n", 1), "merge_rule: auto", "merge_rule: human", 1)
	}
	files["projects/demo-app.yaml"] = []byte(policy.Header("projects/demo-app.yaml") + "\n" + demo)
	files["projects/ai-software-factory-code.yaml"] = []byte(policy.Header("projects/ai-software-factory-code.yaml") + "\n" + codeFile)
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	v := policy.Version{ID: 7, Tree: tree}
	g.r.cfg.Policy = func() policy.Version { return v }
	raw, _ := json.Marshal(Plan{TakeOffMain: takeOff})
	if _, err := g.store.AddPlanRevision(context.Background(), "t1", raw, "r1"); err != nil {
		t.Fatal(err)
	}
	c := Case{ID: 7, TaskID: "t1", Kind: CasePlanConfirmation, Status: CaseAtHuman, Facts: map[string]any{"revision": float64(1), "index": float64(1)}}
	g.store.cases = append(g.store.cases, c)
	return g, c
}

// Act asks the guard for a plan's ask: refused for rung 1 at every level,
// and for rung 2 below full auto and on a revision that takes work off main
// or whose reverts cannot be read, each naming the human's press; at full
// auto on a revision that takes nothing off main rung 2 goes past the
// guard, and its note is recorded as a step on the case, which stays open.
// A plan question follows the same one rule.
func TestActAsksThePlanAnswerGuard(t *testing.T) {
	for _, c := range []struct {
		name, autonomy string
		kind           CaseKind
		rung           int
		takeOff        []string
		noRevision     bool
		want           string
	}{
		{"confirmation at full auto", policy.AutonomyFullAuto, CasePlanConfirmation, 2, nil, false, ""},
		{"confirmation taking work off main", policy.AutonomyFullAuto, CasePlanConfirmation, 2, []string{"api"}, false, offMainWord},
		{"confirmation whose revision cannot be read", policy.AutonomyFullAuto, CasePlanConfirmation, 2, nil, true, offMainWord},
		{"confirmation at autonomous", policy.AutonomyAutonomous, CasePlanConfirmation, 2, nil, false, humanConfirmButton},
		{"confirmation at assisted", policy.AutonomyAssisted, CasePlanConfirmation, 2, nil, false, humanConfirmButton},
		{"question at full auto", policy.AutonomyFullAuto, CasePlanQuestion, 2, nil, false, ""},
		{"question at autonomous", policy.AutonomyAutonomous, CasePlanQuestion, 2, nil, false, belowFullAutoWord},
		{"rung 1's confirmation at full auto", policy.AutonomyFullAuto, CasePlanConfirmation, 1, nil, false, "The check-in decides no plan answer"},
		{"rung 1's question at full auto", policy.AutonomyFullAuto, CasePlanQuestion, 1, nil, false, "The check-in decides no plan answer"},
	} {
		g, k := planAskRig(t, c.autonomy, c.takeOff)
		g.store.cases[len(g.store.cases)-1].Kind = c.kind
		if c.noRevision {
			delete(g.store.plans, "t1")
		}
		by := ChatUser
		if c.rung != 2 {
			by = "factory-agent"
		}
		_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: k.ID, By: by, Rung: c.rung, Res: Resolution{Action: ActNote, Note: "x"}})
		steps := g.store.caseSteps[k.ID]
		if c.want == "" {
			if err != nil || len(steps) != 1 || steps[0].Rung != 2 || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman {
				t.Errorf("%s: Act = %v, steps %+v; want the note recorded on the open case", c.name, err, steps)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || len(steps) != 0 {
			t.Errorf("%s: Act = %v, steps %+v; want the refusal with %q and nothing recorded", c.name, err, steps, c.want)
		}
	}
}

// A replan that takes merged work off main is the human's through Act too:
// refused for rung 2 at every level, a proposal never, and for rung 0 and 1
// (naming no step they lack); rung 3's reaches the plan's replan with
// off_main, which answers that the plan has no card on main. A plain replan
// is not refused for it.
func TestActRefusesAnAgentsReplanOffMain(t *testing.T) {
	const why = "A replan that takes merged work off main is the human's, made with the off-main box of the card's Send back to the planner."
	args := map[string]any{"off_main": true}
	for _, c := range []struct {
		name, autonomy, by string
		rung               int
		attach             bool
	}{
		{"rung 2 at manual", policy.AutonomyManual, ChatUser, 2, true},
		{"rung 2 at assisted", policy.AutonomyAssisted, ChatUser, 2, true},
		{"rung 2 at autonomous", policy.AutonomyAutonomous, ChatUser, 2, true},
		{"rung 2 at full auto", policy.AutonomyFullAuto, ChatUser, 2, true},
	} {
		g, k := planAskRig(t, c.autonomy, nil)
		g.store.cases[len(g.store.cases)-1].Kind = CaseSessionCap
		_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: k.ID, By: c.by, Rung: c.rung, Res: Resolution{Action: ActReplan, Note: "the plan is wrong", Args: args}})
		if err == nil || !strings.HasPrefix(err.Error(), why) || strings.Contains(err.Error(), "attach_analysis") != c.attach || len(g.store.caseSteps[k.ID]) != 0 {
			t.Errorf("%s: Act = %v, steps %v; want the refusal, attach_analysis named: %v, nothing recorded", c.name, err, g.store.caseSteps[k.ID], c.attach)
		}
	}
	for rung, attach := range map[int]bool{0: false, 1: false, 2: true, 3: false} {
		got := OffMainReplanRefusal(rung)
		if (got == "") != (rung == 3) || got != "" && (!strings.HasPrefix(got, why) || strings.Contains(got, "attach_analysis") != attach) {
			t.Errorf("OffMainReplanRefusal(%d) = %q", rung, got)
		}
	}
	g, k := planAskRig(t, policy.AutonomyFullAuto, nil)
	g.store.cases[len(g.store.cases)-1].Kind = CaseSessionCap
	task := g.store.tasks["t1"]
	task.Source, task.Parent, task.PlanKey, task.PlanRevision = SourcePlan, "tp", "k", 1
	g.store.tasks["t1"] = task
	g.store.tasks["tp"] = Task{ID: "tp", Kind: "agent", Repository: task.Repository, IssueNumber: 3, Track: policy.TrackPlan, Status: "done"}
	g.r.timeline = g.tl
	_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: k.ID, By: "sebastian", Rung: 3, Res: Resolution{Action: ActReplan, Note: "x", Args: args}})
	if err == nil || !strings.Contains(err.Error(), "has no card on main: replan without taking work off main.") {
		t.Errorf("rung 3's off-main replan = %v, want it to reach the plan's replan with off_main", err)
	}
}

// On a plan case rung 2 takes the plan's answers through the guard and a
// note; any other action that would resolve the case without answering the
// plan is refused, naming the answer tool.
func TestActOffersAPlanCaseOnlyTheAnswerAndANote(t *testing.T) {
	for _, action := range []string{ActRouteApproval, ActClose, ActReplan} {
		g, k := planAskRig(t, policy.AutonomyFullAuto, nil)
		_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", CaseID: k.ID, By: ChatUser, Rung: 2, Res: Resolution{Action: action, Note: "x"}})
		want := "`case_id` 7 is the plan's plan confirmation: answer it with the `answer` tool (`revision` 1, `question` 1), note it, or hand it up with `attach_analysis`. `" + action + "` answers no plan question without answering the plan."
		if err == nil || err.Error() != want || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman || len(g.store.caseSteps[k.ID]) != 0 {
			t.Errorf("%s: Act = %v, want %q and the case left open", action, err, want)
		}
	}
}
